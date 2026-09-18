// Package task 定义任务状态、事件与唯一合法的状态转移表，不执行任何 I/O。
package task

import (
	"errors"
	"fmt"
)

// State 是持久化在存储中的任务状态名称。
type State string

const (
	Created           State = "CREATED"
	Running           State = "RUNNING"
	WaitingInput      State = "WAITING_INPUT"
	WaitingApproval   State = "WAITING_APPROVAL"
	Completed         State = "COMPLETED"
	Failed            State = "FAILED"
	DeliveryUncertain State = "DELIVERY_UNCERTAIN"
	Closed            State = "CLOSED"
)

// Event 是驱动任务状态变化的事件名称。
type Event string

const (
	Start             Event = "start"
	TurnCompleted     Event = "turn_completed"
	InputRequested    Event = "input_requested"
	ApprovalRequested Event = "approval_requested"
	ApprovalResolved  Event = "approval_resolved"
	Fail              Event = "fail"
	ReplyDispatched   Event = "reply_dispatched"
	DeliveryUnknown   Event = "delivery_unknown"
	DeliveryConfirmed Event = "delivery_confirmed"
	Close             Event = "close"
)

// ErrInvalidTransition 表示当前状态不接受该事件，或状态名未知。
var ErrInvalidTransition = errors.New("invalid task transition")

// transitions 是任务状态机的唯一事实来源；未列出的组合一律非法。
var transitions = map[State]map[Event]State{
	Created:           {Start: Running, Fail: Failed, Close: Closed},
	Running:           {TurnCompleted: Completed, InputRequested: WaitingInput, ApprovalRequested: WaitingApproval, Fail: Failed, DeliveryUnknown: DeliveryUncertain, Close: Closed},
	WaitingInput:      {ReplyDispatched: Running, Fail: Failed, Close: Closed},
	WaitingApproval:   {ApprovalResolved: Running, Fail: Failed, Close: Closed},
	Completed:         {ReplyDispatched: Running, Close: Closed},
	Failed:            {Close: Closed},
	DeliveryUncertain: {DeliveryConfirmed: Running, Close: Closed},
	Closed:            {},
}

// Valid 报告状态名是否属于已知集合，供存储读取时校验。
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

// ResumeAfterUnsent 在确认回复没有送达 Agent 后恢复派发前的空闲状态。
// from 必须是 Running（派发时即确认失败）或 DeliveryUncertain（本地核对为未送达），
// resume 必须是 Completed 或 WaitingInput。
func ResumeAfterUnsent(from, resume State) (State, error) {
	if (from != Running && from != DeliveryUncertain) || (resume != Completed && resume != WaitingInput) {
		return "", fmt.Errorf("%w: %s --resume--> %s", ErrInvalidTransition, from, resume)
	}
	return resume, nil
}

// AcceptsReplies 报告该状态下收到的合法回复是否入队等待；Created、Failed、Closed 返回 false。
func AcceptsReplies(s State) bool {
	switch s {
	case Running, WaitingInput, WaitingApproval, Completed, DeliveryUncertain:
		return true
	}
	return false
}

// CanDispatchReply 报告是否可以立即把队首回复交给 Agent；仅 Completed 与 WaitingInput 返回 true。
func CanDispatchReply(s State) bool {
	return s == Completed || s == WaitingInput
}
