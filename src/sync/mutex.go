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
	state int32     // 表示mutex锁当前的状态，前3位表示状态位，剩下的位表示等待的goroutine的数量
	sema  uint32 	// 信号量，用于唤醒goroutine
}

// A Locker represents an object that can be locked and unlocked.
type Locker interface {
	Lock()
	Unlock()
}

const (
	mutexLocked = 1 << iota // 1 mutex is locked 互斥锁已锁住
	mutexWoken				// 2 互斥锁从正常状态被唤醒
	mutexStarving			// 4 互斥锁进入饥饿模式
	mutexWaiterShift = iota // 3 移位以得到等待队列大小【锁状态占3位】

	// 互斥锁的状态比较复杂，最低三位分别表示 mutexLocked、mutexWoken 和 mutexStarving，剩下的位置用来表示当前有多少个 Goroutine 等待互斥锁的释放。 互斥锁的所有状态位都是 0，int32 中的不同位分别表示了不同的状态：
	//
	//mutexLocked — 表示互斥锁的锁定状态； // 第1个bit
	//mutexWoken — 表示从正常模式被唤醒； // 第2个bit 唤醒标志主要用于标识，当前尝试获取锁的G，是否有正在处于唤醒状态
	//mutexStarving — 当前的互斥锁进入饥饿状态； // 第3个bit
	//waitersCount — 当前互斥锁上等待的 Goroutine 个数； // 剩下的bit
	//注意，前三个，不是互斥的

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
	// Fast path: grab unlocked mutex.
	if atomic.CompareAndSwapInt32(&m.state, 0, mutexLocked) {
		if race.Enabled {
			race.Acquire(unsafe.Pointer(m))
		}
		// 1. 加锁成功，直接返回
		return
	}
	// Slow path (outlined so that the fast path can be inlined)
	// 2. 加锁失败，尝试自旋
	m.lockSlow()
}

func (m *Mutex) lockSlow() {
	// 当前G在队列中的等待时间
	var waitStartTime int64
	// 当前G是否处于饥饿装填
	starving := false
	// 当前G是否被唤醒
	awoke := false
	// 当前G的自旋次数
	iter := 0
	// 当前Mutex的状态
	old := m.state
	for {
		// Don't spin in starvation mode, ownership is handed off to waiters
		// so we won't be able to acquire the mutex anyway.
		// 1. 符合自旋的条件
		// 1.1 state 处于锁状态，且，不处于饥饿状态
		// 1.2 当前G满足以下条件：
		// 		1. 运行在多 CPU 的机器上；
		//		2. 当前 G 为了获取该锁进入自旋的次数小于四次；
		//		3. 当前机器上至少存在一个正在运行的处理器 P 并且处理的运行队列为空；
		if old&(mutexLocked|mutexStarving) == mutexLocked && runtime_canSpin(iter) {

			// 主动旋转很有意义。
			// 尝试设置mutexWoken标志。
			// 以防止Unlock时唤醒其他阻塞的goroutine。
			// Active spinning makes sense.
			// Try to set mutexWoken flag to inform Unlock
			// to not wake other blocked goroutines.
			if !awoke && 								// 当前G处于未唤醒状态
				old&mutexWoken == 0 && 					// 当前Mutex也处于未唤醒状态
				old>>mutexWaiterShift != 0 &&			// 等待队列的G的数量不为0
				atomic.CompareAndSwapInt32(&m.state, old, old|mutexWoken) {   // 设置Mutex的state为唤醒状态
				awoke = true											      // 设置当前G的唤醒状态标志
				// Q: 为什么自旋的时候，要将 Mutex 设置为被唤醒状态?
				// A: 为了在其他G释放锁的时候，不对等待队列中的G进行唤醒 这样自旋的G就能直接拿到锁
			}

			// 进行自旋: 自旋是一种多线程同步机制，当前的线程在进入自旋的过程中，会一直保持CPU的占用，持续检查某个条件是否为真。

			// Q: 处于自旋的G，如何抢到锁？
			// A: 每次自旋完，都会重新判断这三个自旋条件：1.Mutex处于锁状态，2.Mutex不处于饥饿模式，3.当前G可以自旋
			// 只要有一个为false，就不再自旋，而是重新获取Mutex状态，进行判断，尝试加锁 如果加锁成功，则跳出循环 如果加锁失败，则继续尝试自旋
			runtime_doSpin()
			// 记录自旋次数
			iter++
			// 记录Mutex的状态
			old = m.state

			// 跳过下面的逻辑，先去判断是否还满足自旋条件，主要是判断Mutex的状态是否变成无锁，或是饥饿状态
			continue
		}

		// 2. 不符合自旋条件
		// 以下代码 试图修改锁状态

		new := old

		// 不要试图获得饥饿状态的Mutex，新到的G必须排队
		// Don't try to acquire starving mutex, new arriving goroutines must queue.

		// 2.1 Mutex不处于饥饿状态，尝试加锁
		if old&mutexStarving == 0 {
			new |= mutexLocked
		}
		// 2.2 Mutex处于锁状态 或 处于饥饿状态，等待队列的G的数量 +1
		// 如果 2.2 为true，则 3.1 一定为false
		if old&(mutexLocked|mutexStarving) != 0 {
			new += 1 << mutexWaiterShift
		}
		// The current goroutine switches mutex to starvation mode.
		// But if the mutex is currently unlocked, don't do the switch.
		// Unlock expects that starving mutex has waiters, which will not be true in this case.
		// 2.3 当前G处于饥饿状态【说明存在等待G的等待时间超过1毫秒】，且，Mutex处于锁状态【没有给其他的G解锁】。
		// 当前G尝试将Mutex改为饥饿状态，这里也就是为什么Mutex会进入饥饿状态的原因
		if starving && old&mutexLocked != 0 {
			// TODO 这里当前G将Mutex改为饥饿状态
			new |= mutexStarving
		}

		// 2.4 当前G处于唤醒状态
		if awoke {
			// The goroutine has been woken from sleep,
			// so we need to reset the flag in either case.
			if new&mutexWoken == 0 {
				// 如果Mutex不处于唤醒状态，抛异常
				throw("sync: inconsistent mutex state")
			}
			// 重置Mutex的唤醒状态为0
			new &^= mutexWoken
		}

		// 3 实现上面提到的尝试
		if atomic.CompareAndSwapInt32(&m.state, old, new) {
			// TODO 加锁成功
			// 3.1 如果Mutex，原本即不处于加锁状态，也不处于饥饿状态，则这里意味着加锁成功，跳出循环
			if old&(mutexLocked|mutexStarving) == 0 {
				break // locked the mutex with CAS
				// TODO 如果这里一直加锁成功，等待队列中的G就不会被唤醒，也就没办法判断当前G是否处于饥饿状态，也就没办法将Mutex设置为饥饿状态
			}

			// If we were already waiting before, queue at the front of the queue.
			// 3.2 如果之前就处于队列中等待，则进入队列且放到队列头部
			// 如果开始等待时间不为0，则表示已经在等待队列中，则runtime_SemacquireMutex会把当前G放到队列头部
			queueLifo := waitStartTime != 0

			// 3.3 更新 开始等待时间，用于判断当前G是否处于饥饿状态
			if waitStartTime == 0 {
				waitStartTime = runtime_nanotime()
			}

			// 3.4 获取信号量 有可能陷入阻塞等待，即放到等待队列中，然后交出CPU
			// 如果我们没有通过 CAS 获得锁，会调用 sync.runtime_SemacquireMutex 使用信号量，保证资源不会被两个 G 获取。
			// sync.runtime_SemacquireMutex 会在方法中不断调用尝试获取锁，并休眠当前 G，等待信号量的释放。
			// 一旦当前 G 可以获取信号量，它就会立刻返回，sync.Mutex.Lock 方法的剩余代码也会继续执行。
			runtime_SemacquireMutex(&m.sema, queueLifo, 1)

			// 3.5 醒来，判断当前G是否处于饥饿状态，通过当前G在队列中的等待时间是否大于等于1毫秒进行判定
			starving = starving || runtime_nanotime()-waitStartTime > starvationThresholdNs

			old = m.state

			// 3.6 当前锁状态为饥饿状态， 说明当前G已经拥有了获取锁的资格【因为饥饿状态的Mutex会把锁交给等待队列的G】
			if old&mutexStarving != 0 {
				// If this goroutine was woken and mutex is in starvation mode,
				// ownership was handed off to us but mutex is in somewhat inconsistent state: mutexLocked is not set and we are still accounted as waiter.
				// Fix that.
				// 修复bug
				if old&(mutexLocked|mutexWoken) != 0 || old>>mutexWaiterShift == 0 {
					throw("sync: inconsistent mutex state")
				}

				// 3.6.1 等待队列数量减1，同时设为锁定状态
				delta := int32(mutexLocked - 1<<mutexWaiterShift)
				// 3.6.2 若当前G本身不处于饥饿状态【等待时间小于1毫秒】， 或 等待队列中没别人【等待队列中只有当前G】，则将锁置为非饥饿状态【即Mutex退出饥饿状态】
				if !starving || old>>mutexWaiterShift == 1 {
					// Exit starvation mode.
					// Critical to do it here and consider wait time.
					// Starvation mode is so inefficient, that two goroutines
					// can go lock-step infinitely once they switch mutex
					// to starvation mode.
					// TODO 这里，Mutex退出饥饿状态
					delta -= mutexStarving
				}
				atomic.AddInt32(&m.state, delta)
				// TODO 加锁成功
				// 3.6.3 加锁成功，跳出循环
				break
			}

			// 3.7 等待队列中的G，被唤醒后加锁不成功。如果当前锁状态又不处于饥饿状态，则当前G继续尝试自旋
			// 3.7.1 将当前G设置为唤醒状态
			awoke = true
			// 3.7.2 重置当前G的自旋次数
			iter = 0
		} else {
			// 4 实现上面的尝试失败
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

	// Fast path: drop lock bit.
	new := atomic.AddInt32(&m.state, -mutexLocked)
	if new != 0 {
		// Outlined slow path to allow inlining the fast path.
		// To hide unlockSlow during tracing we skip one extra frame when tracing GoUnblock.
		// 2. 解锁失败
		m.unlockSlow(new)
	}
	// 1. state == 0，说明等待队列中没有G在等待，且Mutex原来即不处于饥饿状态，也不处于唤醒状态，而仅仅处于锁定状态，直接返回。【即没有G在等待这个锁，也没有G刚好进来要获取锁】
}

func (m *Mutex) unlockSlow(new int32) {
	if (new+mutexLocked)&mutexLocked == 0 {
		throw("sync: unlock of unlocked mutex")
	}
	// 1. Mutex处于正常模式
	if new&mutexStarving == 0 {
		old := new
		for {
			// 1.1  如果没有等待的G 或者 Mutex已经被唤醒，不需要叫醒任何等待G。【对应的是解锁的自旋G会将Mutex设置为唤醒状态】
			// 在饥饿模式下，所有权直接从解锁goroutine传递给下一个等待者。
			// 我们不是这个链条的一部分，因为当我们解锁上面的互斥锁时，我们没有观察到mutexStarving。 所以下车吧。
			// If there are no waiters or a goroutine has already been woken or grabbed the lock, no need to wake anyone.
			// In starvation mode ownership is directly handed off from unlocking goroutine to the next waiter.
			// We are not part of this chain, since we did not observe mutexStarving when we unlocked the mutex above.
			// So get off the way.
			if old>>mutexWaiterShift == 0 || old&(mutexLocked|mutexWoken|mutexStarving) != 0 {
				return
			}
			// 抓住权利，唤醒某个G
			// Grab the right to wake someone.
			// 1.2 将Mutex设置为唤醒状态，同事将等待队列的数量 -1
			new = (old - 1<<mutexWaiterShift) | mutexWoken
			// 1.3 更新1.2的设置，并通过 sync.runtime_Semrelease 唤醒等待者并移交锁的所有权；
			// runtime_Semrelease 和 runtime_SemacquireMutex 对应
			if atomic.CompareAndSwapInt32(&m.state, old, new) {
				// TODO 这里唤醒等待队列中的G
				runtime_Semrelease(&m.sema, false, 1)
				return
			}
			old = m.state
		}
	} else {
		// 2. Mutex存于饥饿模式

		// 饥饿模式：将互斥锁权限移交给下一个等待G，以及放弃当前G的时间片，以致于等待G可以获取时间片立即执行
		// 注意：未设置mutexLocked，唤醒G将在唤醒后设置它。
		// 但是如果设置了mutexStarving，仍然认为互斥锁是锁定的，
		// 所以新来的G不会获得它。
		// Starving mode: handoff mutex ownership to the next waiter, and yield our time slice so that the next waiter can start to run immediately.
		// Note: mutexLocked is not set, the waiter will set it after wakeup.
		// But mutex is still considered locked if mutexStarving is set,
		// so new coming goroutines won't acquire it.

		// 调用 sync.runtime_Semrelease 方法将当前锁交给下一个正在尝试获取锁的等待者，等待者被唤醒后会得到锁，在这时互斥锁还不会退出饥饿状态；
		runtime_Semrelease(&m.sema, true, 1)
	}
}
