// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package sync provides basic synchronization primitives such as mutual
// exclusion locks. Other than the Once and WaitGroup types, most are intended
// for use by low-level library routines. Higher-level synchronization is
// better done via channels and communication.
//
// Values containing the types defined in this package should not be copied.
package sync

import (
	"internal/race"
	"sync/atomic"
	"unsafe"
)

func throw(string) // provided by runtime

// A Mutex is a mutual exclusion lock.
// The zero value for a Mutex is an unlocked mutex.
//
// A Mutex must not be copied after first use.
type Mutex struct {
	// 最低三位分别表示 mutexLocked、mutexWoken 和 mutexStarving
	// 剩下的位置用来表示当前有多少个 Goroutine 等待互斥锁的释放
	// mutexLocked — 表示互斥锁的锁定状态；
	// mutexWoken — 表示从正常模式被唤醒；
	// mutexStarving — 当前的互斥锁进入饥饿状态；
	// waitersCount — 当前互斥锁上等待的 Goroutine 个数
	state int32    // 表示当前互斥锁的状态
	sema  uint32   // 用于控制锁状态的信号量
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // mutex is locked
	mutexWoken
	mutexStarving
	mutexWaiterShift = iota

	// Mutex fairness.
	//
	// Mutex can be in 2 modes of operations: normal and starvation.
	// In normal mode waiters are queued in FIFO order, but a woken up waiter
	// does not own the mutex and competes with new arriving goroutines over
	// the ownership. New arriving goroutines have an advantage -- they are
	// already running on CPU and there can be lots of them, so a woken up
	// waiter has good chances of losing. In such case it is queued at front
	// of the wait queue. If a waiter fails to acquire the mutex for more than 1ms,
	// it switches mutex to the starvation mode.
	//
	// In starvation mode ownership of the mutex is directly handed off from
	// the unlocking goroutine to the waiter at the front of the queue.
	// New arriving goroutines don't try to acquire the mutex even if it appears
	// to be unlocked, and don't try to spin. Instead they queue themselves at
	// the tail of the wait queue.
	//
	// If a waiter receives ownership of the mutex and sees that either
	// (1) it is the last waiter in the queue, or (2) it waited for less than 1 ms,
	// it switches mutex back to normal operation mode.
	//
	// Normal mode has considerably better performance as a goroutine can acquire
	// a mutex several times in a row even if there are blocked waiters.
	// Starvation mode is important to prevent pathological cases of tail latency.
	starvationThresholdNs = 1e6
)

// Lock locks m.
// If the lock is already in use, the calling goroutine
// blocks until the mutex is available.
func (m *Mutex) Lock() {
	// 1.如果当前Mutex的锁状态是0，直接将state置为1
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		return
	}
	// 2.如果当前Mutex的锁状态不是0，调用lockSlow，尝试通过自旋等方式等待锁的释放。
	// Slow path (outlined so that the fast path can be inlined)
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	var waitStartTime int64
	starving := false
	awoke := false
	iter := 0
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 一、自旋
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {
			// 1.Mutex处于锁状态，2.Mutex不处于饥饿模式，3.当前G可以自旋

			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.

			// 1.awoke 							当前G不处于被唤醒状态
			// 2.old&mutexWoken == 0			当前没有其他正在唤醒的G
			// 3.old>>mutexWaiterShift != 0		当前有正在等待的G 【右移3位，如果不位0，则表明当前有正在等待的goroutine】

			// 4.atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken)  将Mutex的状态未知唤醒状态
			// 5.awoke = true					将当前G设置为被唤醒的G
			if !awoke && old&mutexWoken == 0 && old>>mutexWaiterShift != 0 &&
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {
				awoke = true
			}
			// 总结：每个没有获取到锁，且进入自旋状态的G，都会被尝试设置成被唤醒的G。这也是等待队列中的G被唤醒后很难抢赢新创建的G的原因

			// 尝试自旋
			runtime_doSpin()
			// 自旋计数，最多4次
			iter++
			// 重新获取Mutex的状态
			old = m.state
			continue
		}

		// 二、更新Mutex的状态
		// Mutex不处于锁状态 或者 自旋超过指定次数
		new := old
		// Don't try to acquire starving mutex, new arriving goroutines must queue.
		if old&mutexStarving == 0 {
			// TODO 加锁操作
			// 1) 如果当前不是饥饿模式，则这里其实就可以尝试进行 加锁操作
			// |=其实就是将锁的那个bit位设为1表示锁定状态
			new |= mutexLocked
		}
		if old&(mutexLocked|mutexStarving) != 0 {
			// 2) 如果当前被锁定 或者 处于饥饿模式，则等待计数器加1
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not
		// be true in this case.
		if starving && old&mutexLocked != 0 {
			// 3）如果当前已经处于饥饿状态，并且当前锁还是被占用，则尝试进行饥饿模式的切换
			new |= mutexStarving
		}

		if awoke {
			// 4) 当前G是被唤醒状态，awoke为true则表明当前线程在上面自旋的时候，修改mutexWoken状态成功
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				// 当前G的唤醒状态和Mutex的唤醒状态不一致
				throw("sync: inconsistent mutex state")
			}
			// 清理Mutex的唤醒标志位
			// Q: 为什么要清理唤醒标志位呢？
			// A: 实际上是因为后续流程很有可能当前G会被挂起,就需要等待其他释放锁的G来唤醒
			//    但如果其他G unlock的时候发现 mutexWoken 的位置不是0，则就不会去唤醒，则该线程就无法再醒来加锁
			new &^= mutexWoken
		}

		// 三、加锁
		// 再加锁的时候实际上只会有一个goroutine加锁CAS成功，而其他线程则需要重新获取状态，进行上面的自旋与唤醒状态的重新计算，从而再次CAS
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// 一）更新Mutex状态成功
			if old&(mutexLocked|mutexStarving) == 0 {
				// 1.已经释放了锁
				// 2.Mutex不处于饥饿状态
				// 表示当前G 已经进行完了加锁操作了
				break // locked the mutex with CAS
			}
			// If we were already waiting before, queue at the front of the queue.
			// 排队逻辑，如果发现waitStatrTime不为0，则表明当前线程之前已经再排队来，后面可能因为
			// unlock被唤醒，但是本次依旧没获取到锁，所以就将它移动到等待队列的头部
			queueLifo := waitStartTime != 0
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}

			// 这里就会进行排队，等待其他节点进行 唤醒
			// TODO 休眠当前G，将其放入等待队列
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// 如果等待超过指定时间，则切换为饥饿模式 starving=true
			// 如果一个线程之前不是饥饿状态，并且也没超过starvationThresholdNs，则starving为false
			// 就会触发下面的状态切换
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs
			// 重新获取状态
			old = m.state
			if old&mutexStarving != 0 {
				// 如果发现当前已经是饥饿模式，注意饥饿模式唤醒的是第一个goroutine

				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat
				// inconsistent state: mutexLocked is not set and we are still
				// accounted as waiter. Fix that.

				// 一致性检查，
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}
				// 获取当前的模式
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				break
			}
			awoke = true
			iter = 0
		} else {
			// 二）更新Mutex状态失败
			// 重新获取Mutex状态，继续尝试进行自旋
			old = m.state
		}
	}

	if race.Enabled {
		race.Acquire(unsafe.Pointer(m))
	}
}

// Unlock unlocks m.
// It is a run-time error if m is not locked on entry to Unlock.
//
// A locked Mutex is not associated with a particular goroutine.
// It is allowed for one goroutine to lock a Mutex and then
// arrange for another goroutine to unlock it.
func (m *Mutex) Unlock() {
	if race.Enabled {
		_ = m.state
		race.Release(unsafe.Pointer(m))
	}
	// 直接进行CAS操作
	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		m.unlockSlow(new)
	}
}

func (m *Mutex) unlockSlow(new int32) {
	// 方法首先会校验锁状态的合法性 — 如果当前互斥锁已经被解锁过了就会直接抛出异常 sync: unlock of unlocked mutex 中止当前程序。
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	if new&mutexStarving == 0 {
		// 非饥饿模式
		old := new
		for {
			// If there are no waiters or a goroutine has already
			// been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking
			// goroutine to the next waiter. We are not part of this chain,
			// since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				// 如果已经有等待的G，并且已经被唤醒，就直接返回
				// 这里返回，等待队列的G就没有机会被唤醒
				return
			}
			// Grab the right to wake someone.
			// 减去一个等待计数，然后将当前模式切换成mutexWoken
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// TODO 唤醒等待队列的一个G
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// 饥饿模式
		// Starving mode: handoff mutex ownership to the next waiter, and yield
		// our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.
		// TODO 唤醒等待队列的一个G
		runtime_Semrelease(&m.sema, true, 1)
	}
}
