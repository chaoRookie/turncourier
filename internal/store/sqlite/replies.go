// Package sqlite 在一个事务中完成入站回复的去重、入站记录与回复队列项的写入；只保存元数据与正文摘要，不保存正文。
package sqlite

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chaoRookie/turncourier/internal/queue"
	"github.com/chaoRookie/turncourier/internal/task"
)

// InboundReply 是已通过发件人、线程与令牌校验的入站回复元数据；不含正文。
type InboundReply struct {
	TaskID      string
	Account     string // 调用方已用 config.NormalizeAddress 规范化的机器人邮箱地址
	UIDValidity uint32
	UID         uint32
	MessageID   string // 与邮件头一致的原样字符串
	BodySHA256  [32]byte
}

// Reply 是回复队列项的持久化快照；Seq 即本地入队顺序。
type Reply struct {
	Seq          int64
	TaskID       string
	State        queue.State
	ResumeState  task.State // 仅在派发后记录派发前的任务状态
	RejectReason string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// RecordResult 说明 RecordReply 是新入队还是命中了已有记录。
type RecordResult struct {
	Reply     Reply
	Duplicate bool
}

// ErrMessageConflict 表示同一邮件标识对应了不同内容或不同 Message-ID，必须拒绝并告警。
var ErrMessageConflict = errors.New("inbound message conflict")

// stateRejectReasons 是不接受回复的任务状态对应的拒绝原因；未列出的状态记为 task_not_accepting。
var stateRejectReasons = map[task.State]string{
	task.Failed: "task_failed",
	task.Closed: "task_closed",
}

// knownInbound 是按唯一键找到的已有入站记录中参与去重判定的字段，以及它对应的回复序号。
type knownInbound struct {
	messageID string
	digest    []byte
	taskID    string
	seq       int64
}

// RecordReply 在同一事务中完成去重检查、写入入站记录与写入回复队列项。
// 任务处于 AcceptsReplies 为 true 的状态时入队为 QUEUED；否则记录为 REJECTED，原因为 task_not_accepting、task_failed 或 task_closed。
// 同一账户中 (UIDVALIDITY, UID) 或 Message-ID 已有记录时，Message-ID、正文摘要与任务都一致才算重复，
// 返回原回复且不写入；任一不一致返回 ErrMessageConflict。重复邮件不按任务的当前状态重新判定。
func (s *Store) RecordReply(ctx context.Context, in InboundReply) (RecordResult, error) {
	if err := in.validate(); err != nil {
		return RecordResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordResult{}, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	current, err := getTask(ctx, tx, in.TaskID)
	if err != nil {
		return RecordResult{}, err
	}
	byUID, err := findInbound(ctx, tx, "account = ? AND uid_validity = ? AND uid = ?", in.Account, in.UIDValidity, in.UID)
	if err != nil {
		return RecordResult{}, err
	}
	byMessageID, err := findInbound(ctx, tx, "account = ? AND message_id = ?", in.Account, in.MessageID)
	if err != nil {
		return RecordResult{}, err
	}
	for _, known := range []*knownInbound{byUID, byMessageID} {
		if known != nil && (known.messageID != in.MessageID || !bytes.Equal(known.digest, in.BodySHA256[:]) || known.taskID != in.TaskID) {
			return RecordResult{}, fmt.Errorf("%w: uid %d/%d or its message id is already recorded with a different message id, digest or task", ErrMessageConflict, in.UIDValidity, in.UID)
		}
	}
	// Message-ID 在账户内唯一，两个查找都命中且一致时是同一条记录。
	if known := cmp.Or(byUID, byMessageID); known != nil {
		reply, err := getReply(ctx, tx, known.seq)
		return RecordResult{Reply: reply, Duplicate: true}, err
	}

	state := queue.Queued
	var reason sql.NullString
	if !task.AcceptsReplies(current.State) {
		if state, err = queue.Next(queue.Queued, queue.Reject); err != nil {
			return RecordResult{}, err
		}
		reason = sql.NullString{String: cmp.Or(stateRejectReasons[current.State], "task_not_accepting"), Valid: true}
	}
	now := s.now().UnixMilli()
	var inboundID, seq int64
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO inbound_messages (account, uid_validity, uid, message_id, body_sha256, task_id, received_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id",
		in.Account, in.UIDValidity, in.UID, in.MessageID, in.BodySHA256[:], in.TaskID, now).Scan(&inboundID); err != nil {
		return RecordResult{}, fmt.Errorf("cannot record inbound message: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO replies (inbound_id, task_id, state, reject_reason, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING seq",
		inboundID, in.TaskID, string(state), reason, now, now).Scan(&seq); err != nil {
		return RecordResult{}, fmt.Errorf("cannot enqueue reply: %w", err)
	}
	reply, err := getReply(ctx, tx, seq)
	if err != nil {
		return RecordResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RecordResult{}, fmt.Errorf("cannot commit reply: %w", err)
	}
	return RecordResult{Reply: reply}, nil
}

// validate 在事务开始前检查与表约束对应的长度、空白与取值范围；存储层不导入 config，地址规范化由调用方负责。
// 长度按 Unicode 字符计，与表约束中 SQLite 的 length() 一致；length() 遇到 NUL 即停止计数，因此含 NUL 的值一律拒绝。
func (in InboundReply) validate() error {
	accountLen, messageIDLen := utf8.RuneCountInString(in.Account), utf8.RuneCountInString(in.MessageID)
	switch {
	case accountLen < 3 || accountLen > 254 || strings.ContainsFunc(in.Account, unicode.IsSpace):
		return errors.New("invalid inbound reply: account must be 3-254 characters without whitespace")
	case in.UIDValidity == 0 || in.UID == 0:
		return errors.New("invalid inbound reply: uid validity and uid must be positive")
	case messageIDLen < 3 || messageIDLen > 998:
		return errors.New("invalid inbound reply: message id must be 3-998 characters")
	case strings.ContainsRune(in.Account, 0) || strings.ContainsRune(in.MessageID, 0):
		return errors.New("invalid inbound reply: account and message id must not contain NUL")
	}
	return nil
}

// findInbound 按 where 条件（某个唯一键）查找已有入站记录及其回复序号；where 只来自本文件的常量，未找到时返回 nil。
func findInbound(ctx context.Context, tx *sql.Tx, where string, args ...any) (*knownInbound, error) {
	var known knownInbound
	err := tx.QueryRowContext(ctx,
		"SELECT i.message_id, i.body_sha256, i.task_id, r.seq FROM inbound_messages i JOIN replies r ON r.inbound_id = i.id WHERE "+where, args...).
		Scan(&known.messageID, &known.digest, &known.taskID, &known.seq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read inbound messages: %w", err)
	}
	return &known, nil
}

// getReply 通过 q 按序号读取回复快照，时间换算为 UTC；回复状态或派发前任务状态不在已知集合时返回错误而不是静默接受。
func getReply(ctx context.Context, q rowQuerier, seq int64) (Reply, error) {
	var r Reply
	var resumeState, rejectReason sql.NullString
	var createdAt, updatedAt int64
	err := q.QueryRowContext(ctx,
		"SELECT seq, task_id, state, resume_state, reject_reason, created_at, updated_at FROM replies WHERE seq = ?", seq).
		Scan(&r.Seq, &r.TaskID, &r.State, &resumeState, &rejectReason, &createdAt, &updatedAt)
	if err != nil {
		return Reply{}, fmt.Errorf("cannot read reply: %w", err)
	}
	if !r.State.Valid() {
		return Reply{}, fmt.Errorf("reply %d has unknown state %q", seq, r.State)
	}
	r.ResumeState = task.State(resumeState.String)
	if resumeState.Valid && !r.ResumeState.Valid() {
		return Reply{}, fmt.Errorf("reply %d has unknown resume state %q", seq, r.ResumeState)
	}
	r.RejectReason = rejectReason.String
	r.CreatedAt = time.UnixMilli(createdAt).UTC()
	r.UpdatedAt = time.UnixMilli(updatedAt).UTC()
	return r, nil
}
