// Package queue 定义待发通知的状态、事件与转移表；它与回复队列并列，互不复用转移表，不执行任何 I/O。
package queue

import (
	"errors"
	"fmt"
)

// OutboxState 是待发通知的持久化状态名称。
type OutboxState string

const (
	OutboxPending   OutboxState = "PENDING"   // 等待发送，内容密文已落盘
	OutboxSending   OutboxState = "SENDING"   // 已由唯一的发送进程领取，SMTP 会话进行中
	OutboxSent      OutboxState = "SENT"      // 服务器以 250 接受，或本地核对为已投递
	OutboxUncertain OutboxState = "UNCERTAIN" // SMTP 已进入提交阶段（结束标记可能已写出）但未得到响应，或发送中进程崩溃
	OutboxAbandoned OutboxState = "ABANDONED" // 放弃：服务器永久拒绝、任务已关闭，或本地核对后决定不再发送
)

// OutboxEvent 是驱动待发通知状态变化的事件名称。
type OutboxEvent string

const (
	OutboxClaim         OutboxEvent = "claim"
	OutboxDelivered     OutboxEvent = "delivered"
	OutboxMarkUncertain OutboxEvent = "mark_uncertain"
	OutboxRequeue       OutboxEvent = "requeue"
	OutboxAbandon       OutboxEvent = "abandon"
)

// ErrInvalidOutboxTransition 表示待发通知当前状态不接受该事件，或状态名未知。
var ErrInvalidOutboxTransition = errors.New("invalid outbox transition")

// outboxTransitions 是待发通知状态机的唯一事实来源；SENT 与 ABANDONED 为终态。
var outboxTransitions = map[OutboxState]map[OutboxEvent]OutboxState{
	OutboxPending:   {OutboxClaim: OutboxSending, OutboxAbandon: OutboxAbandoned},
	OutboxSending:   {OutboxDelivered: OutboxSent, OutboxMarkUncertain: OutboxUncertain, OutboxRequeue: OutboxPending, OutboxAbandon: OutboxAbandoned},
	OutboxUncertain: {OutboxDelivered: OutboxSent, OutboxRequeue: OutboxPending, OutboxAbandon: OutboxAbandoned},
	OutboxSent:      {},
	OutboxAbandoned: {},
}

// Valid 报告状态名是否属于已知集合。
func (s OutboxState) Valid() bool {
	_, ok := outboxTransitions[s]
	return ok
}

// NextOutbox 返回事件发生后的状态；错误包装 ErrInvalidOutboxTransition 并写明状态与事件名。
// 未知状态在转移表中查不到任何事件，同样按非法转移处理。
func NextOutbox(from OutboxState, event OutboxEvent) (OutboxState, error) {
	if to, ok := outboxTransitions[from][event]; ok {
		return to, nil
	}
	return "", fmt.Errorf("%w: %s --%s-->", ErrInvalidOutboxTransition, from, event)
}
