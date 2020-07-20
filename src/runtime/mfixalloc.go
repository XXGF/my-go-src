// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Fixed-size object allocator. Returned memory is not zeroed.
//
// See malloc.go for overview.

package runtime

import "unsafe"

// FixAlloc is a simple free-list allocator for fixed size objects.
// fixalloc 是一个简单的固定大小对象的自由表内存分配器。

// Malloc uses a FixAlloc wrapped around sysAlloc to manage its
// mcache and mspan objects.
// Malloc 使用围绕 sysAlloc 的 fixalloc 来管理其 MCache 和 MSpan 对象。

//
// Memory returned by fixalloc.alloc is zeroed by default, but the
// caller may take responsibility for zeroing allocations by setting
// the zero flag to false. This is only safe if the memory never
// contains heap pointers.
// fixalloc.alloc 返回的内存默认为零，但调用者可以通过将 zero 标志设置为 false
// 来自行负责将分配归零。如果这部分内存永远不包含堆指针，则这样的操作是安全的。

//
// The caller is responsible for locking around FixAlloc calls.
// Callers can keep state in the object but the first word is
// smashed by freeing and reallocating.
// 调用方负责锁定 fixalloc 调用。调用方可以在对象中保持状态，
// 但当释放和重新分配时第一个字会被破坏。

//
// Consider marking fixalloc'd types go:notinheap.
//  考虑使 fixalloc 的类型变为 go:notinheap.

// fixalloc 是一个基于自由列表的固定大小的分配器。其核心原理是将若干未分配的内存块连接起来， 将未分配的区域的第一个字为指向下一个未分配区域的指针使用。
//Go 的主分配堆中 malloc（span、cache、treap、finalizer、profile、arena hint 等） 均 围绕它为实体进行固定分配和回收。
//fixalloc 作为抽象，非常简洁，只包含三个基本操作：初始化、分配、回收
type fixalloc struct {
	size   uintptr
	first  func(arg, p unsafe.Pointer) // called first time p is returned
	arg    unsafe.Pointer
	list   *mlink
	chunk  uintptr // use uintptr instead of unsafe.Pointer to avoid write barriers
	nchunk uint32
	inuse  uintptr // in-use bytes now
	stat   *uint64
	zero   bool // zero allocations
}

// A generic linked list of blocks.  (Typically the block is bigger than sizeof(MLink).)
// Since assignments to mlink.next will result in a write barrier being performed
// this cannot be used by some of the internal GC structures. For example when
// the sweeper is placing an unmarked object on the free list it does not want the
// write barrier to be called since that could result in the object being reachable.
//
//go:notinheap
type mlink struct {
	next *mlink
}

// Initialize f to allocate objects of the given size,
// using the allocator to obtain chunks of memory.
func (f *fixalloc) init(size uintptr, first func(arg, p unsafe.Pointer), arg unsafe.Pointer, stat *uint64) {
	f.size = size
	f.first = first
	f.arg = arg
	f.list = nil
	f.chunk = 0
	f.nchunk = 0
	f.inuse = 0
	f.stat = stat
	f.zero = true
}

// 获取下一个空闲的内存空间
func (f *fixalloc) alloc() unsafe.Pointer {
	if f.size == 0 {
		print("runtime: use of FixAlloc_Alloc before FixAlloc_Init\n")
		throw("runtime: internal error")
	}

	// 如果f.list不是nil，则说明还存在已经释放、可复用的内存，直接将其分配
	if f.list != nil {
		// 获取内存块首地址
		v := unsafe.Pointer(f.list)
		// 将list指向下一段区域
		f.list = f.list.next
		// 增加已使用内存大小
		f.inuse += f.size
		// 如果需要对内存清零，则对取出的内存执行初始化
		if f.zero {
			memclrNoHeapPointers(v, f.size)
		}
		// 返回分配的内存
		return v
	}

	// f.list中没有可复用的内存

	// 如果此时nchunk不足以分配一个size
	if uintptr(f.nchunk) < f.size {
		// 则向操作系统申请内存，大小为 16 << 10，即pow(2,14)
		f.chunk = uintptr(persistentalloc(_FixAllocChunk, 0, f.stat))
		f.nchunk = _FixAllocChunk
	}

	// 指向申请好的内存
	v := unsafe.Pointer(f.chunk)
	if f.first != nil {
		f.first(f.arg, v)
	}
	// 增加已分配的内存块
	f.chunk = f.chunk + f.size
	// 减少未分配的内存块
	f.nchunk -= uint32(f.size)
	// 增加已使用的内存大小
	f.inuse += f.size
	return v
}

// 释放指针指向的内存空间
// 直接将回收的地址指针，放回到自由表中【自由表指的是fixalloc】
func (f *fixalloc) free(p unsafe.Pointer) {
	f.inuse -= f.size
	v := (*mlink)(p)
	v.next = f.list
	f.list = v
}
