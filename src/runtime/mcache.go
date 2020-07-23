// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

// Per-thread (in Go, per-P) cache for small objects.
// No locking needed because it is per-thread (per-P).
//
// mcaches are allocated from non-GC'd memory, so any heap pointers
// must be specially handled.
//
// 是一个 per-P 的缓存，它是一个包含不同大小等级的 span 链表的数组，
// 其中 mcache.alloc 的每一个数组元素 都是某一个特定大小的 mspan 的链表头指针。

// 1.mcache 会被 P 持有，当 M 和 P 绑定时，M 同样会保留 mcache 的指针
// 2.mcache 直接向操作系统申请内存，且常驻运行时
// 3.P通过make命令进行分配，会分配在Go堆上
//go:notinheap
type mcache struct {
	// The following members are accessed on every malloc,
	// so they are grouped here for better caching.
	next_sample uintptr // trigger heap sample after allocating this many bytes
	// 分配的可扫描的字节数
	local_scan  uintptr // bytes of scannable heap allocated

	// Allocator cache for tiny objects w/o pointers.
	// See "Tiny allocator" comment in malloc.go.

	// tiny points to the beginning of the current tiny block, or
	// nil if there is no current tiny block.
	//
	// tiny is a heap pointer. Since mcache is in non-GC'd memory,
	// we handle it by clearing it in releaseAll during mark
	// termination.

	// tiny 指向当前 tiny 块的起始位置，或当没有 tiny 块时候为 nil
	// tiny 是一个堆指针。由于mcache在非GC内存中，我们通弄过在 mark termination 期间，在releaseAll 中清楚它来处理它
	tiny             uintptr
	// 下一个空闲内存所在的偏移量
	tinyoffset       uintptr
	// 记录内存分配器中分配的对象个数。
	local_tinyallocs uintptr     // number of tiny allocs not counted in other stats

	// 以上字段会在每次alloc时，都被访问

	// The rest is not accessed on every malloc.

	// 每一个线程缓存都持有 67 * 2 个 runtime.mspan，这些内存管理单元都存储在结构体的 alloc 字段中：
	alloc [numSpanClasses]*mspan // spans to allocate from, indexed by spanClass  // 用来分配的 spans，由 spanClass 索引

	stackcache [_NumStackOrders]stackfreelist    // 栈内存由于与线程关系比较密切，所以我们在每一个线程缓存 runtime.mcache 中都加入了栈缓存减少锁竞争影响。

	// Local allocator stats, flushed during GC.
	// 本地分配器统计，在 GC 期间被刷新
	local_largefree  uintptr                  // bytes freed for large objects (>maxsmallsize)
	local_nlargefree uintptr                  // number of frees for large objects (>maxsmallsize)
	local_nsmallfree [_NumSizeClasses]uintptr // number of frees for small objects (<=maxsmallsize)

	// flushGen indicates the sweepgen during which this mcache
	// was last flushed. If flushGen != mheap_.sweepgen, the spans
	// in this mcache are stale and need to the flushed so they
	// can be swept. This is done in acquirep.
	flushGen uint32
}

// A gclink is a node in a linked list of blocks, like mlink,
// but it is opaque to the garbage collector.
// The GC does not trace the pointers during collection,
// and the compiler does not emit write barriers for assignments
// of gclinkptr values. Code should store references to gclinks
// as gclinkptr, not as *gclink.
type gclink struct {
	next gclinkptr
}

// A gclinkptr is a pointer to a gclink, but it is opaque
// to the garbage collector.
type gclinkptr uintptr

// ptr returns the *gclink form of p.
// The result should be used for accessing fields, not stored
// in other data structures.
func (p gclinkptr) ptr() *gclink {
	return (*gclink)(unsafe.Pointer(p))
}

type stackfreelist struct {
	list gclinkptr // linked list of free stacks
	size uintptr   // total size of stacks in list
}

// dummy mspan that contains no free objects.
// 虚拟的MSpan，不包含任何对象。
var emptymspan mspan

// 运行时在初始化处理器时【即mallocinit】，会调用 runtime.allocmcache 初始化线程缓存，
// 该函数会在系统栈中使用 runtime.mheap 中的线程缓存分配器初始化新的 runtime.mcache 结构体：
func allocmcache() *mcache {
	var c *mcache
	systemstack(func() {
		lock(&mheap_.lock)
		// 从 mheap 上分配一个 mcache。 由于 mheap 是全局的，因此在分配期必须对其进行加锁。
		// 使用 fixalloc组件 来完成分配
		c = (*mcache)(mheap_.cachealloc.alloc())
		c.flushGen = mheap_.sweepgen
		unlock(&mheap_.lock)
	})
	// c.alloc 存放的是 *mspan
	for i := range c.alloc {
		// 初始化后的runtime.mcache中的所有runtime.mspan都是空的占位符
		c.alloc[i] = &emptymspan
	}
	// 返回下一个采样点，是服从泊松过程的随机数
	c.next_sample = nextSample()
	return c
}

// 释放mcache
func freemcache(c *mcache) {
	systemstack(func() {
		// 归还mspan
		c.releaseAll()
		// 释放stack
		stackcache_clear(c)

		// NOTE(rsc,rlh): If gcworkbuffree comes back, we need to coordinate
		// with the stealing of gcworkbufs during garbage collection to avoid
		// a race where the workbuf is double-freed.
		// gcworkbuffree(c.gcworkbuf)

		lock(&mheap_.lock)
		// 记录局部统计
		purgecachedstats(c)
		// 将mcache释放
		mheap_.cachealloc.free(unsafe.Pointer(c))
		unlock(&mheap_.lock)
	})
}

// refill acquires a new span of span class spc for c. This span will
// have at least one free object. The current span in c must be full.
//
// Must run in a non-preemptible context since otherwise the owner of
// c could change.
func (c *mcache) refill(spc spanClass) {
	// Return the current cached span to the central lists.
	s := c.alloc[spc]

	if uintptr(s.allocCount) != s.nelems {
		throw("refill of span with free space remaining")
	}
	if s != &emptymspan {
		// Mark this span as no longer cached.
		if s.sweepgen != mheap_.sweepgen+3 {
			throw("bad sweepgen in refill")
		}
		if go115NewMCentralImpl {
			mheap_.central[spc].mcentral.uncacheSpan(s)
		} else {
			atomic.Store(&s.sweepgen, mheap_.sweepgen)
		}
	}

	// Get a new cached span from the central lists.
	// 该函数会从中心缓存中申请新的 runtime.mspan 存储到线程缓存中，
	// 这也是向线程缓存中插入内存管理单元的唯一方法。
	// 线程缓存【即mcacne】会通过 runtime.mcentral.cacheSpan 方法获取新的内存管理单元
	s = mheap_.central[spc].mcentral.cacheSpan()
	if s == nil {
		throw("out of memory")
	}

	if uintptr(s.allocCount) == s.nelems {
		throw("span has no free space")
	}

	// Indicate that this span is cached and prevent asynchronous
	// sweeping in the next sweep phase.
	s.sweepgen = mheap_.sweepgen + 3

	c.alloc[spc] = s
}

// 由于mcache从非GC内存上进行分配，因此出现的任何对指针，都必须进行特殊处理。
// 所以在释放前，需要调用mcache.releaseAll将堆指针进行处理：
func (c *mcache) releaseAll() {
	for i := range c.alloc {
		s := c.alloc[i]
		if s != &emptymspan {
			// 将mspan归还
			mheap_.central[i].mcentral.uncacheSpan(s)
			c.alloc[i] = &emptymspan
		}
	}
	// Clear tinyalloc pool.
	// 清空tinyalloc池
	c.tiny = 0
	c.tinyoffset = 0
}

// prepareForSweep flushes c if the system has entered a new sweep phase
// since c was populated. This must happen between the sweep phase
// starting and the first allocation from c.
func (c *mcache) prepareForSweep() {
	// Alternatively, instead of making sure we do this on every P
	// between starting the world and allocating on that P, we
	// could leave allocate-black on, allow allocation to continue
	// as usual, use a ragged barrier at the beginning of sweep to
	// ensure all cached spans are swept, and then disable
	// allocate-black. However, with this approach it's difficult
	// to avoid spilling mark bits into the *next* GC cycle.
	sg := mheap_.sweepgen
	if c.flushGen == sg {
		return
	} else if c.flushGen != sg-2 {
		println("bad flushGen", c.flushGen, "in prepareForSweep; sweepgen", sg)
		throw("bad flushGen")
	}
	c.releaseAll()
	stackcache_clear(c)
	atomic.Store(&c.flushGen, mheap_.sweepgen) // Synchronizes with gcStart
}
