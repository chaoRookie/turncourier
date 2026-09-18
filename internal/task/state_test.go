// Package task 用独立维护的规格副本穷举验证任务状态机，不引用实现中的转移表。
package task

import (
	"errors"
	"strings"
	"testing"
)

// allStates 按规格列出全部 8 个任务状态，用于穷举矩阵。
var allStates = []State{Created, Running, WaitingInput, WaitingApproval, Completed, Failed, DeliveryUncertain, Closed}

// allEvents 按规格列出全部 10 个任务事件，用于穷举矩阵。
var allEvents = []Event{Start, TurnCompleted, InputRequested, ApprovalRequested, ApprovalResolved, Fail, ReplyDispatched, DeliveryUnknown, DeliveryConfirmed, Close}

// TestPersistedNames 确认状态与事件的字符串值与规格一致；这些值会写入存储，改名会破坏已有数据。
func TestPersistedNames(t *testing.T) {
	states := map[State]string{
		Created: "CREATED", Running: "RUNNING", WaitingInput: "WAITING_INPUT", WaitingApproval: "WAITING_APPROVAL",
		Completed: "COMPLETED", Failed: "FAILED", DeliveryUncertain: "DELIVERY_UNCERTAIN", Closed: "CLOSED",
	}
	for _, state := range allStates {
		if string(state) != states[state] {
			t.Errorf("state %q, want %q", state, states[state])
		}
	}
	events := map[Event]string{
		Start: "start", TurnCompleted: "turn_completed", InputRequested: "input_requested", ApprovalRequested: "approval_requested",
		ApprovalResolved: "approval_resolved", Fail: "fail", ReplyDispatched: "reply_dispatched", DeliveryUnknown: "delivery_unknown",
		DeliveryConfirmed: "delivery_confirmed", Close: "close",
	}
	for _, event := range allEvents {
		if string(event) != events[event] {
			t.Errorf("event %q, want %q", event, events[event])
		}
	}
}

// TestNextMatrix 对 8 个状态 × 10 个事件逐一调用 Next：期望矩阵中的组合必须到达目标状态，
// 其余组合必须返回 ErrInvalidTransition，且错误文本同时写明状态名与事件名。
func TestNextMatrix(t *testing.T) {
	want := map[State]map[Event]State{
		Created:           {Start: Running, Fail: Failed, Close: Closed},
		Running:           {TurnCompleted: Completed, InputRequested: WaitingInput, ApprovalRequested: WaitingApproval, Fail: Failed, DeliveryUnknown: DeliveryUncertain, Close: Closed},
		WaitingInput:      {ReplyDispatched: Running, Fail: Failed, Close: Closed},
		WaitingApproval:   {ApprovalResolved: Running, Fail: Failed, Close: Closed},
		Completed:         {ReplyDispatched: Running, Close: Closed},
		Failed:            {Close: Closed},
		DeliveryUncertain: {DeliveryConfirmed: Running, Close: Closed},
		Closed:            {},
	}
	for _, from := range allStates {
		for _, event := range allEvents {
			got, err := Next(from, event)
			if to, ok := want[from][event]; ok {
				if err != nil || got != to {
					t.Errorf("Next(%s, %s) = %q, %v; want %s", from, event, got, err, to)
				}
				continue
			}
			assertInvalid(t, err, string(from), string(event))
		}
	}
}

// TestNextUnknownState 确认未知状态名不会被当作任何合法状态处理。
func TestNextUnknownState(t *testing.T) {
	_, err := Next("BOGUS", Start)
	assertInvalid(t, err, "BOGUS", string(Start))
}

// TestNextUnknownEvent 确认已知状态收到未知事件时同样返回 ErrInvalidTransition，错误文本写明状态名与事件名；
// 穷举矩阵只含已知事件，覆盖不到这一情形。
func TestNextUnknownEvent(t *testing.T) {
	for _, from := range allStates {
		_, err := Next(from, "bogus")
		assertInvalid(t, err, string(from), "bogus")
	}
}

// TestClosedIsTerminal 确认 Closed 是终态，任何事件都不能离开。
func TestClosedIsTerminal(t *testing.T) {
	for _, event := range allEvents {
		_, err := Next(Closed, event)
		assertInvalid(t, err, string(Closed), string(event))
	}
}

// TestResumeAfterUnsent 对 8 个已知状态加一个未知状态穷举 from × resume：只有派发中或投递不确定的任务
// 能恢复，且只能回到 Completed 或 WaitingInput；其余组合必须返回 ErrInvalidTransition 并写明两个状态名。
func TestResumeAfterUnsent(t *testing.T) {
	states := append(allStates, "BOGUS")
	for _, from := range states {
		for _, resume := range states {
			got, err := ResumeAfterUnsent(from, resume)
			if (from == Running || from == DeliveryUncertain) && (resume == Completed || resume == WaitingInput) {
				if err != nil || got != resume {
					t.Errorf("ResumeAfterUnsent(%s, %s) = %q, %v; want %s", from, resume, got, err, resume)
				}
				continue
			}
			assertInvalid(t, err, string(from), string(resume))
		}
	}
}

// TestAcceptsReplies 确认未启动、失败和关闭的任务不接收回复，未知状态同样拒绝。
func TestAcceptsReplies(t *testing.T) {
	want := map[State]bool{
		Created: false, Running: true, WaitingInput: true, WaitingApproval: true,
		Completed: true, Failed: false, DeliveryUncertain: true, Closed: false, "BOGUS": false,
	}
	for state, accepts := range want {
		if got := AcceptsReplies(state); got != accepts {
			t.Errorf("AcceptsReplies(%s) = %v, want %v", state, got, accepts)
		}
	}
}

// TestCanDispatchReply 确认只有空闲等待下一轮的 Completed 与 WaitingInput 可以立即派发回复。
func TestCanDispatchReply(t *testing.T) {
	for _, state := range append(allStates, "BOGUS") {
		want := state == Completed || state == WaitingInput
		if got := CanDispatchReply(state); got != want {
			t.Errorf("CanDispatchReply(%s) = %v, want %v", state, got, want)
		}
	}
}

// TestValid 确认只有规格中的 8 个状态名合法，空串与大小写不同的名称均不合法。
func TestValid(t *testing.T) {
	for _, state := range allStates {
		if !state.Valid() {
			t.Errorf("%q should be valid", state)
		}
	}
	for _, state := range []State{"", "running"} {
		if state.Valid() {
			t.Errorf("%q should be invalid", state)
		}
	}
}

// assertInvalid 断言错误包装 ErrInvalidTransition，并在文本中写明所有相关名称，便于排查。
func assertInvalid(t *testing.T, err error, names ...string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("err = %v, want ErrInvalidTransition", err)
		return
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %q", err, name)
		}
	}
}
