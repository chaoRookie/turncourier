// Package queue 定义回复队列项的状态、事件与转移表；队列状态独立于任务状态，不执行任何 I/O。
package queue

import (
	"errors"
	"fmt"
)

// State 是回复队列项的持久化状态名称。
type State string

const (
	Queued       State = "QUEUED"
	Dispatching  State = "DISPATCHING"
	Acknowledged State = "ACKNOWLEDGED"
	Uncertain    State = "UNCERTAIN"
	Rejected     State = "REJECTED"
)

// Event 是驱动队列项状态变化的事件名称。
type Event string

const (
	Claim         Event = "claim"
	Acknowledge   Event = "acknowledge"
	MarkUncertain Event = "mark_uncertain"
	Requeue       Event = "requeue"
	Reject        Event = "reject"
)

// ErrInvalidTransition 表示队列项当前状态不接受该事件，或状态名未知。
var ErrInvalidTransition = errors.New("invalid reply queue transition")

// transitions 是队列状态机的唯一事实来源；Acknowledged 与 Rejected 为终态。
var transitions = map[State]map[Event]State{
	Queued:       {Claim: Dispatching, Reject: Rejected},
	Dispatching:  {Acknowledge: Acknowledged, MarkUncertain: Uncertain, Requeue: Queued, Reject: Rejected},
	Uncertain:    {Acknowledge: Acknowledged, Requeue: Queued, Reject: Rejected},
	Acknowledged: {},
	Rejected:     {},
}

// Valid 报告状态名是否属于已知集合。
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// Next 返回事件发生后的状态；错误包装 ErrInvalidTransition 并写明状态与事件名。
// 未知状态在转移表中查不到任何事件，同样按非法转移处理。
func Next(from State, event Event) (State, error) {
	if to, ok := transitions[from][event]; ok {
		return to, nil
	}
	return "", fmt.Errorf("%w: %s --%s-->", ErrInvalidTransition, from, event)
}
