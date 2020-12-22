// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

// There is a modified copy of this file in runtime/rwmutex.go.
// If you make any changes here, see if you should make them there.

// A RWMutex is a reader/writer mutual exclusion lock.
// The lock can be held by an arbitrary number of readers or a single writer.
// The zero value for a RWMutex is an unlocked mutex.
//
// A RWMutex must not be copied after first use.
//
// If a goroutine holds a RWMutex for reading and another goroutine might
// call Lock, no goroutine should expect to be able to acquire a read lock
// until the initial read lock is released. In particular, this prohibits
// recursive read locking. This is to ensure that the lock eventually becomes
// available; a blocked Lock call excludes new readers from acquiring the
// lock.
type RWMutex struct {
	// 写锁复用互斥锁
	w           Mutex  // held if there are pending writers
	// 写锁信号量
	writerSem   uint32 // semaphore for writers to wait for completing readers
	// 读锁信号量
	readerSem   uint32 // semaphore for readers to wait for completing writers

	// 1. 不考虑写操作的情况下，每次读锁定将该值+1，每次解除读锁定将该值-1，所以readerCount取值为[0, N]，N为读者个数，实际上最大可支持2^30个并发读者。
	// 2. 写锁如何阻塞读锁：当写锁定进行时，会先将readerCount减去2^30，从而readerCount变成了负值，此时再有读锁定到来时检测到readerCount为负值，便知道有写操作在进行，只好阻塞等待。而真实的读操作个数并不会丢失，只需要将readerCount加上2^30即可获得。
	// 3. 读锁如何阻塞写锁：读锁定会先将readerCount +1，此时写操作到来时发现读者数量不为0，会阻塞等待所有读操作结束。
	readerCount int32  // number of pending readers		// 读锁计数器

	// Q：为什么写锁定不会被饿死？
	// A: 写操作要等待读操作结束后才可以获得锁，写操作等待期间可能还有新的读操作持续到来，如果写操作等待所有读操作结束，很可能被饿死。
	//    通过RWMutex.readerWait可完美解决这个问题。
	//    写操作到来时，会把RWMutex.readerCount值拷贝到RWMutex.readerWait中，用于标记排在写操作前面的读者个数。
	//    前面的读操作结束后，除了会递减RWMutex.readerCount，还会递减RWMutex.readerWait值，当RWMutex.readerWait值变为0时唤醒写操作。
	//    所以，写操作就相当于把一段连续的读操作划分成两部分，前面的读操作结束后唤醒写操作，写操作结束后唤醒后面的读操作。
	readerWait  int32  // number of departing readers   // 获取写锁的时候，需要等待 readerWait 为0
}

// 支持最多2^30个读锁
const rwmutexMaxReaders = 1 << 30

// RLock locks rw for reading.
//
// It should not be used for recursive read locking; a blocked Lock
// call excludes new readers from acquiring the lock. See the
// documentation on the RWMutex type.
func (rw *RWMutex) RLock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// 1. 每次 goroutine 获取读锁时，readerCount+1
	// 2. 如果写锁已经被获取，那么 readerCount 在 -rwmutexMaxReaders 与 0 之间【将RWMutex的Lock方法】，这时挂起获取读锁的 goroutine
	// 3. 如果写锁没有被获取，那么 readerCount > 0，获取读锁, 不阻塞
	// 4. 通过 readerCount 判断读锁与写锁互斥, 如果有写锁存在就挂起goroutine, 多个读锁可以并行
	if atomic.AddInt32(&rw.readerCount, 1) < 0 {
		// 即使存在写锁 readerCount会正常增长，而readerWait则不会增长。readerWait记录的是写锁之前的读锁

		// A writer is pending, wait for it.
		// 写锁阻塞了读锁：读锁等待
		runtime_SemacquireMutex(&rw.readerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
	}
}

// RUnlock undoes a single RLock call;
// it does not affect other simultaneous readers.
// It is a run-time error if rw is not locked for reading
// on entry to RUnlock.
func (rw *RWMutex) RUnlock() {
	if race.Enabled {
		_ = rw.w.state
		race.ReleaseMerge(unsafe.Pointer(&rw.writerSem))
		race.Disable()
	}
	// 这里分两种情况
	// 1. 非 写锁等待或锁定状态
	//    r + 1 >= 0, 直接解锁完毕

	// 2. 写锁等待或锁定状态
	// 1.1 r + 1 == -rwmutexMaxReaders，说明有写锁处于等待或锁定状态
	if r := atomic.AddInt32(&rw.readerCount, -1); r < 0 {
		// Outlined slow-path to allow the fast-path to be inlined
		rw.rUnlockSlow(r)
	}
	if race.Enabled {
		race.Enable()
	}
}

func (rw *RWMutex) rUnlockSlow(r int32) {
	if r+1 == 0 || r+1 == -rwmutexMaxReaders {
		race.Enable()
		throw("sync: RUnlock of unlocked RWMutex")
	}
	// A writer is pending.
	// 1. 写锁锁定状态：直接解锁完毕
	// 2. 写锁等待状态，且 写锁之前的读锁只剩当前的读锁，即 readerWait -1 之后等于0，唤醒等待的写锁
	if atomic.AddInt32(&rw.readerWait, -1) == 0 {
		// The last reader unblocks the writer.
		// 唤醒等待的写锁
		runtime_Semrelease(&rw.writerSem, false, 1)
	}
}

// Lock locks rw for writing.
// If the lock is already locked for reading or writing,
// Lock blocks until the lock is available.
func (rw *RWMutex) Lock() {
	if race.Enabled {
		_ = rw.w.state
		race.Disable()
	}
	// First, resolve competition with other writers.
	// 1. 调用 Mutex 的Lock方法，阻塞后续的写操做
	rw.w.Lock()
	// Announce to readers there is a pending writer.
	// 2. 将当前的 readerCount 置为负数，告诉 RLock 当前存在写锁等待，阻塞后续的读锁。【r 是 readerCount 的正确值】
	r := atomic.AddInt32(&rw.readerCount, -rwmutexMaxReaders) + rwmutexMaxReaders
	// Wait for active readers.
	// 3. 将当前数量的读锁【即写操做之前的读锁】，存在 readerWait 中，写锁只需要等到 readerWait 数量的读写都结束，就可以醒来
	//    readerWait保证了写锁不会被饿死
	if r != 0 && atomic.AddInt32(&rw.readerWait, r) != 0 {
		// 读锁阻塞了写锁：写锁等待
		runtime_SemacquireMutex(&rw.writerSem, false, 0)
	}
	if race.Enabled {
		race.Enable()
		race.Acquire(unsafe.Pointer(&rw.readerSem))
		race.Acquire(unsafe.Pointer(&rw.writerSem))
	}
}

// Unlock unlocks rw for writing. It is a run-time error if rw is
// not locked for writing on entry to Unlock.
//
// As with Mutexes, a locked RWMutex is not associated with a particular
// goroutine. One goroutine may RLock (Lock) a RWMutex and then
// arrange for another goroutine to RUnlock (Unlock) it.
func (rw *RWMutex) Unlock() {
	if race.Enabled {
		_ = rw.w.state
		race.Release(unsafe.Pointer(&rw.readerSem))
		race.Disable()
	}

	// Announce to readers there is no active writer.
	// 1. 加上 Lock 的时候减去的 rwmutexMaxReaders
	r := atomic.AddInt32(&rw.readerCount, rwmutexMaxReaders)
	// 2. 没执行Lock调用Unlock，抛出异常
	if r >= rwmutexMaxReaders {
		race.Enable()
		throw("sync: Unlock of unlocked RWMutex")
	}
	// Unblock blocked readers, if any.
	// 3. 通知当前等待的读锁
	for i := 0; i < int(r); i++ {
		// 唤醒当前阻塞的读锁
		runtime_Semrelease(&rw.readerSem, false, 0)
	}
	// Allow other writers to proceed.
	// 4. 释放 Mutex 锁
	rw.w.Unlock()
	if race.Enabled {
		race.Enable()
	}
}

// RLocker returns a Locker interface that implements
// the Lock and Unlock methods by calling rw.RLock and rw.RUnlock.
func (rw *RWMutex) RLocker() Locker {
	return (*rlocker)(rw)
}

type rlocker RWMutex

func (r *rlocker) Lock()   { (*RWMutex)(r).RLock() }
func (r *rlocker) Unlock() { (*RWMutex)(r).RUnlock() }
