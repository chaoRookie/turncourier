// Package queue 用独立维护的规格副本穷举验证待发通知状态机，不引用实现中的转移表，并确认它与回复队列的转移表互不共用。
package queue

import (
	"errors"
	"strings"
	"testing"
)

// allOutboxStates 按规格列出全部 5 个待发通知状态，用于穷举矩阵。
var allOutboxStates = []OutboxState{OutboxPending, OutboxSending, OutboxSent, OutboxUncertain, OutboxAbandoned}

// allOutboxEvents 按规格列出全部 5 个待发通知事件，用于穷举矩阵。
var allOutboxEvents = []OutboxEvent{OutboxClaim, OutboxDelivered, OutboxMarkUncertain, OutboxRequeue, OutboxAbandon}

// TestOutboxPersistedNames 确认待发通知状态与事件的字符串值与规格一致；状态值写入存储并受表约束检查，改名会破坏已有数据。
func TestOutboxPersistedNames(t *testing.T) {
	states := map[OutboxState]string{
		OutboxPending: "PENDING", OutboxSending: "SENDING", OutboxSent: "SENT",
		OutboxUncertain: "UNCERTAIN", OutboxAbandoned: "ABANDONED",
	}
	for _, state := range allOutboxStates {
		if string(state) != states[state] {
			t.Errorf("state %q, want %q", state, states[state])
		}
	}
	events := map[OutboxEvent]string{
		OutboxClaim: "claim", OutboxDelivered: "delivered", OutboxMarkUncertain: "mark_uncertain",
		OutboxRequeue: "requeue", OutboxAbandon: "abandon",
	}
	for _, event := range allOutboxEvents {
		if string(event) != events[event] {
			t.Errorf("event %q, want %q", event, events[event])
		}
	}
}

// TestNextOutboxMatrix 对 5 个状态 × 5 个事件逐一调用 NextOutbox：期望矩阵中的组合必须到达目标状态，
// 其余组合必须返回 ErrInvalidOutboxTransition，且错误文本同时写明状态名与事件名。
func TestNextOutboxMatrix(t *testing.T) {
	want := map[OutboxState]map[OutboxEvent]OutboxState{
		OutboxPending:   {OutboxClaim: OutboxSending, OutboxAbandon: OutboxAbandoned},
		OutboxSending:   {OutboxDelivered: OutboxSent, OutboxMarkUncertain: OutboxUncertain, OutboxRequeue: OutboxPending, OutboxAbandon: OutboxAbandoned},
		OutboxUncertain: {OutboxDelivered: OutboxSent, OutboxRequeue: OutboxPending, OutboxAbandon: OutboxAbandoned},
		OutboxSent:      {},
		OutboxAbandoned: {},
	}
	for _, from := range allOutboxStates {
		for _, event := range allOutboxEvents {
			got, err := NextOutbox(from, event)
			if to, ok := want[from][event]; ok {
				if err != nil || got != to {
					t.Errorf("NextOutbox(%s, %s) = %q, %v; want %s", from, event, got, err, to)
				}
				continue
			}
			if got != "" {
				t.Errorf("NextOutbox(%s, %s) = %q on error, want empty state", from, event, got)
			}
			assertInvalidOutbox(t, err, string(from), string(event))
		}
	}
}

// TestNextOutboxUnknownState 确认未知状态名不会被当作任何合法状态处理，包括空串与小写形式。
func TestNextOutboxUnknownState(t *testing.T) {
	for _, from := range []OutboxState{"", "pending", "BOGUS"} {
		_, err := NextOutbox(from, OutboxClaim)
		assertInvalidOutbox(t, err, string(from), string(OutboxClaim))
	}
}

// TestNextOutboxUnknownEvent 确认已知状态收到未知事件时同样返回 ErrInvalidOutboxTransition，错误文本写明状态名与 bogus；
// 穷举矩阵只含已知事件，覆盖不到这一情形。
func TestNextOutboxUnknownEvent(t *testing.T) {
	for _, from := range allOutboxStates {
		_, err := NextOutbox(from, "bogus")
		assertInvalidOutbox(t, err, string(from), "bogus")
	}
}

// TestOutboxTerminalStates 确认 SENT 与 ABANDONED 是终态，任何事件都不能离开。
func TestOutboxTerminalStates(t *testing.T) {
	for _, from := range []OutboxState{OutboxSent, OutboxAbandoned} {
		for _, event := range allOutboxEvents {
			_, err := NextOutbox(from, event)
			assertInvalidOutbox(t, err, string(from), string(event))
		}
	}
}

// TestOutboxStateValid 确认只有规格中的 5 个状态名合法；空串、小写名称与回复队列的状态名均不合法。
func TestOutboxStateValid(t *testing.T) {
	for _, state := range allOutboxStates {
		if !state.Valid() {
			t.Errorf("%q should be valid", state)
		}
	}
	for _, state := range []OutboxState{"", "pending", "QUEUED"} {
		if state.Valid() {
			t.Errorf("%q should be invalid", state)
		}
	}
}

// TestTransitionTablesSeparate 确认两张转移表互不共用：待发通知状态机不认回复队列的状态，回复队列状态机也不认待发通知的状态，
// 且各自只返回自己的非法转移错误。
func TestTransitionTablesSeparate(t *testing.T) {
	_, err := NextOutbox(OutboxState("QUEUED"), OutboxClaim)
	assertInvalidOutbox(t, err, "QUEUED", string(OutboxClaim))
	if errors.Is(err, ErrInvalidTransition) {
		t.Errorf("outbox error %v must not wrap ErrInvalidTransition", err)
	}
	_, err = Next(State("PENDING"), Claim)
	assertInvalid(t, err, "PENDING", string(Claim))
	if errors.Is(err, ErrInvalidOutboxTransition) {
		t.Errorf("reply queue error %v must not wrap ErrInvalidOutboxTransition", err)
	}
	if State("PENDING").Valid() {
		t.Error(`State("PENDING") should be invalid for the reply queue`)
	}
}

// assertInvalidOutbox 断言错误包装 ErrInvalidOutboxTransition，并在文本中写明所有相关名称，便于排查。
func assertInvalidOutbox(t *testing.T, err error, names ...string) {
	t.Helper()
	if !errors.Is(err, ErrInvalidOutboxTransition) {
		t.Errorf("err = %v, want ErrInvalidOutboxTransition", err)
		return
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %q", err, name)
		}
	}
}
