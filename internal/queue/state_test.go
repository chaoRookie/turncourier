// Package queue 用独立维护的规格副本穷举验证回复队列状态机，不引用实现中的转移表。
package queue

import (
	"errors"
	"strings"
	"testing"
)

// allStates 按规格列出全部 5 个队列状态，用于穷举矩阵。
var allStates = []State{Queued, Dispatching, Acknowledged, Uncertain, Rejected}

// allEvents 按规格列出全部 5 个队列事件，用于穷举矩阵。
var allEvents = []Event{Claim, Acknowledge, MarkUncertain, Requeue, Reject}

// TestPersistedNames 确认状态与事件的字符串值与规格一致；状态值写入存储并受表约束检查，改名会破坏已有数据。
func TestPersistedNames(t *testing.T) {
	states := map[State]string{
		Queued: "QUEUED", Dispatching: "DISPATCHING", Acknowledged: "ACKNOWLEDGED",
		Uncertain: "UNCERTAIN", Rejected: "REJECTED",
	}
	for _, state := range allStates {
		if string(state) != states[state] {
			t.Errorf("state %q, want %q", state, states[state])
		}
	}
	events := map[Event]string{
		Claim: "claim", Acknowledge: "acknowledge", MarkUncertain: "mark_uncertain",
		Requeue: "requeue", Reject: "reject",
	}
	for _, event := range allEvents {
		if string(event) != events[event] {
			t.Errorf("event %q, want %q", event, events[event])
		}
	}
}

// TestNextMatrix 对 5 个状态 × 5 个事件逐一调用 Next：期望矩阵中的组合必须到达目标状态，
// 其余组合必须返回 ErrInvalidTransition，且错误文本同时写明状态名与事件名。
func TestNextMatrix(t *testing.T) {
	want := map[State]map[Event]State{
		Queued:       {Claim: Dispatching, Reject: Rejected},
		Dispatching:  {Acknowledge: Acknowledged, MarkUncertain: Uncertain, Requeue: Queued, Reject: Rejected},
		Uncertain:    {Acknowledge: Acknowledged, Requeue: Queued, Reject: Rejected},
		Acknowledged: {},
		Rejected:     {},
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
	_, err := Next("BOGUS", Claim)
	assertInvalid(t, err, "BOGUS", string(Claim))
}

// TestTerminalStates 确认 Acknowledged 与 Rejected 是终态，任何事件都不能离开。
func TestTerminalStates(t *testing.T) {
	for _, from := range []State{Acknowledged, Rejected} {
		for _, event := range allEvents {
			_, err := Next(from, event)
			assertInvalid(t, err, string(from), string(event))
		}
	}
}

// TestValid 确认只有规格中的 5 个状态名合法，空串与大小写不同的名称均不合法。
func TestValid(t *testing.T) {
	for _, state := range allStates {
		if !state.Valid() {
			t.Errorf("%q should be valid", state)
		}
	}
	for _, state := range []State{"", "queued", "BOGUS"} {
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
