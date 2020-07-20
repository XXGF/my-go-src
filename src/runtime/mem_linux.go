// Copyright 2010 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package runtime

import (
	"runtime/internal/atomic"
	"unsafe"
)

const (
	_EACCES = 13
	_EINVAL = 22
)

// 概述：
// 系统级的内存管理调用是平台相关的，这里以 Linux 为例，

// 运行时的 sysAlloc、sysUnused、sysUsed、sysFree、sysReserve、sysMap 和 sysFault 都是系统级的调用。

// 其中 sysAlloc、sysReserve 和 sysMap 都是向操作系统申请内存的操作，他们均涉及关于内存分配的系统调用就是 mmap，
//区别在于：
//sysAlloc 是从操作系统上申请清零后的内存，调用参数是 _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_PRIVATE；
//sysReserve 是从操作系统中保留内存的地址空间，并未直接分配内存，调用参数是 _PROT_NONE, _MAP_ANON|_MAP_PRIVATE，；
//sysMap 则是用于通知操作系统使用先前已经保留好的空间，参数是 _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_FIXED|_MAP_PRIVATE。

// 不过 sysAlloc 和 sysReserve 都是操作系统对齐的内存，但堆分配器可能使用更大的对齐方式，因此这部分获得的内存都需要额外进行一些重排的工作。

// Don't split the stack as this method may be invoked without a valid G, which
// prevents us from allocating more stack.
//go:nosplit
func sysAlloc(n uintptr, sysStat *uint64) unsafe.Pointer {
	p, err := mmap(nil, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		if err == _EACCES {
			print("runtime: mmap: access denied\n")
			exit(2)
		}
		if err == _EAGAIN {
			print("runtime: mmap: too much locked memory (check 'ulimit -l').\n")
			exit(2)
		}
		return nil
	}
	mSysStatInc(sysStat, n)
	return p
}

var adviseUnused = uint32(_MADV_FREE)

func sysUnused(v unsafe.Pointer, n uintptr) {
	// By default, Linux's "transparent huge page" support will
	// merge pages into a huge page if there's even a single
	// present regular page, undoing the effects of madvise(adviseUnused)
	// below. On amd64, that means khugepaged can turn a single
	// 4KB page to 2MB, bloating the process's RSS by as much as
	// 512X. (See issue #8832 and Linux kernel bug
	// https://bugzilla.kernel.org/show_bug.cgi?id=93111)
	//
	// To work around this, we explicitly disable transparent huge
	// pages when we release pages of the heap. However, we have
	// to do this carefully because changing this flag tends to
	// split the VMA (memory mapping) containing v in to three
	// VMAs in order to track the different values of the
	// MADV_NOHUGEPAGE flag in the different regions. There's a
	// default limit of 65530 VMAs per address space (sysctl
	// vm.max_map_count), so we must be careful not to create too
	// many VMAs (see issue #12233).
	//
	// Since huge pages are huge, there's little use in adjusting
	// the MADV_NOHUGEPAGE flag on a fine granularity, so we avoid
	// exploding the number of VMAs by only adjusting the
	// MADV_NOHUGEPAGE flag on a large granularity. This still
	// gets most of the benefit of huge pages while keeping the
	// number of VMAs under control. With hugePageSize = 2MB, even
	// a pessimal heap can reach 128GB before running out of VMAs.
	if physHugePageSize != 0 {
		// If it's a large allocation, we want to leave huge
		// pages enabled. Hence, we only adjust the huge page
		// flag on the huge pages containing v and v+n-1, and
		// only if those aren't aligned.
		var head, tail uintptr
		if uintptr(v)&(physHugePageSize-1) != 0 {
			// Compute huge page containing v.
			head = alignDown(uintptr(v), physHugePageSize)
		}
		if (uintptr(v)+n)&(physHugePageSize-1) != 0 {
			// Compute huge page containing v+n-1.
			tail = alignDown(uintptr(v)+n-1, physHugePageSize)
		}

		// Note that madvise will return EINVAL if the flag is
		// already set, which is quite likely. We ignore
		// errors.
		if head != 0 && head+physHugePageSize == tail {
			// head and tail are different but adjacent,
			// so do this in one call.
			madvise(unsafe.Pointer(head), 2*physHugePageSize, _MADV_NOHUGEPAGE)
		} else {
			// Advise the huge pages containing v and v+n-1.
			if head != 0 {
				madvise(unsafe.Pointer(head), physHugePageSize, _MADV_NOHUGEPAGE)
			}
			if tail != 0 && tail != head {
				madvise(unsafe.Pointer(tail), physHugePageSize, _MADV_NOHUGEPAGE)
			}
		}
	}

	if uintptr(v)&(physPageSize-1) != 0 || n&(physPageSize-1) != 0 {
		// madvise will round this to any physical page
		// *covered* by this range, so an unaligned madvise
		// will release more memory than intended.
		throw("unaligned sysUnused")
	}

	var advise uint32
	if debug.madvdontneed != 0 {
		advise = _MADV_DONTNEED
	} else {
		advise = atomic.Load(&adviseUnused)
	}
	if errno := madvise(v, n, int32(advise)); advise == _MADV_FREE && errno != 0 {
		// MADV_FREE was added in Linux 4.5. Fall back to MADV_DONTNEED if it is
		// not supported.
		atomic.Store(&adviseUnused, _MADV_DONTNEED)
		madvise(v, n, _MADV_DONTNEED)
	}
}

func sysUsed(v unsafe.Pointer, n uintptr) {
	// Partially undo the NOHUGEPAGE marks from sysUnused
	// for whole huge pages between v and v+n. This may
	// leave huge pages off at the end points v and v+n
	// even though allocations may cover these entire huge
	// pages. We could detect this and undo NOHUGEPAGE on
	// the end points as well, but it's probably not worth
	// the cost because when neighboring allocations are
	// freed sysUnused will just set NOHUGEPAGE again.
	sysHugePage(v, n)
}

func sysHugePage(v unsafe.Pointer, n uintptr) {
	if physHugePageSize != 0 {
		// Round v up to a huge page boundary.
		beg := alignUp(uintptr(v), physHugePageSize)
		// Round v+n down to a huge page boundary.
		end := alignDown(uintptr(v)+n, physHugePageSize)

		if beg < end {
			madvise(unsafe.Pointer(beg), end-beg, _MADV_HUGEPAGE)
		}
	}
}

// Don't split the stack as this function may be invoked without a valid G,
// which prevents us from allocating more stack.
//go:nosplit
func sysFree(v unsafe.Pointer, n uintptr, sysStat *uint64) {
	mSysStatDec(sysStat, n)
	munmap(v, n)
}

func sysFault(v unsafe.Pointer, n uintptr) {
	mmap(v, n, _PROT_NONE, _MAP_ANON|_MAP_PRIVATE|_MAP_FIXED, -1, 0)
}

func sysReserve(v unsafe.Pointer, n uintptr) unsafe.Pointer {
	p, err := mmap(v, n, _PROT_NONE, _MAP_ANON|_MAP_PRIVATE, -1, 0)
	if err != 0 {
		return nil
	}
	return p
}

func sysMap(v unsafe.Pointer, n uintptr, sysStat *uint64) {
	mSysStatInc(sysStat, n)

	p, err := mmap(v, n, _PROT_READ|_PROT_WRITE, _MAP_ANON|_MAP_FIXED|_MAP_PRIVATE, -1, 0)
	if err == _ENOMEM {
		throw("runtime: out of memory")
	}
	if p != v || err != 0 {
		throw("runtime: cannot map pages in arena address space")
	}
}

// 总结：
// 最后我们来理一下这些系统级调用的关系：
//
//当开始保留内存地址时，调用 sysReserve；
//当需要使用或不使用保留的内存区域时，通知操作系统，调用 sysUnused、sysUsed；

//正式使用保留的地址，使用 sysMap；
//释放时使用 sysFree 以及调试时使用 sysFault；

//非用户态的调试、“堆外内存“ 则使用 sysAlloc 直接向操作系统获得清零的内存。
