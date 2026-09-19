// Package sqlite 保存 IMAP 收取游标与被拒来信的元数据：游标在同一 UIDVALIDITY 下只进不退，UIDVALIDITY 变化时整体重置；
// 被拒来信只记录元数据与原因码，不含主题与正文，也从不回信（防回环与反向散射）。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Cursor 是某个文件夹的收取游标；LastUID 为 0 表示该 UIDVALIDITY 下尚未处理任何邮件。
type Cursor struct {
	UIDValidity uint32
	LastUID     uint32
}

// ErrCursorRegression 表示在 UIDVALIDITY 不变时试图把 LastUID 调小。
var ErrCursorRegression = errors.New("fetch cursor would move backwards")

// RejectReason 是被拒来信的原因码；合法取值只由下列常量定义，4b 可增补。
type RejectReason string

const (
	RejectAutoReply        RejectReason = "auto_reply"
	RejectBounce           RejectReason = "bounce"
	RejectSenderNotAllowed RejectReason = "sender_not_allowed"
	RejectTooLarge         RejectReason = "too_large"
	RejectMalformed        RejectReason = "malformed"
	RejectParseUncertain   RejectReason = "parse_uncertain"
	RejectThreadMismatch   RejectReason = "thread_mismatch"
	RejectSubjectTag       RejectReason = "subject_tag_invalid"
	RejectTokenMissing     RejectReason = "token_missing"
	RejectTokenInvalid     RejectReason = "token_invalid"
	RejectTokenExpired     RejectReason = "token_expired"
	RejectTaskUnknown      RejectReason = "task_unknown"
	RejectMessageConflict  RejectReason = "message_conflict"
)

// knownRejectReasons 是 RecordRejection 接受的原因码，即上列全部常量。数据库只约束形状（1–40 个 [a-z_] 字符），
// 增补原因码只需在这里与上面的常量中同时加上，不需要迁移。
var knownRejectReasons = map[RejectReason]bool{
	RejectAutoReply:        true,
	RejectBounce:           true,
	RejectSenderNotAllowed: true,
	RejectTooLarge:         true,
	RejectMalformed:        true,
	RejectParseUncertain:   true,
	RejectThreadMismatch:   true,
	RejectSubjectTag:       true,
	RejectTokenMissing:     true,
	RejectTokenInvalid:     true,
	RejectTokenExpired:     true,
	RejectTaskUnknown:      true,
	RejectMessageConflict:  true,
}

// maxRejectionsLimit 是 Rejections 一次最多返回的条数。
const maxRejectionsLimit = 1000

// Rejection 是一封被拒来信的元数据，不含主题与正文。ID 与 ReceivedAt 由存储填写：RecordRejection 忽略输入中的这两个字段，
// 分别取数据库分配的 ID 与存储时钟的当前时间（UTC 毫秒）。
type Rejection struct {
	ID          int64
	Account     string
	Folder      string
	UIDValidity uint32
	UID         uint32
	MessageID   string // 可为空
	Sender      string // 调用方规范化后的发件人地址，可为空
	Reason      RejectReason
	TaskID      string // 能识别任务时填写，可为空
	ReceivedAt  time.Time
}

// FetchCursor 读取 (account, folder) 的游标；没有记录时返回 ErrNotFound。account 与 folder 按 checkMailbox 的规则校验，
// 不合法时在查询前报错。
func (s *Store) FetchCursor(ctx context.Context, account, folder string) (Cursor, error) {
	if err := checkMailbox(account, folder); err != nil {
		return Cursor{}, fmt.Errorf("invalid fetch cursor: %w", err)
	}
	var c Cursor
	err := s.db.QueryRowContext(ctx, "SELECT uid_validity, last_uid FROM fetch_cursors WHERE account = ? AND folder = ?", account, folder).
		Scan(&c.UIDValidity, &c.LastUID)
	if errors.Is(err, sql.ErrNoRows) {
		return Cursor{}, fmt.Errorf("fetch cursor: %w", ErrNotFound)
	}
	if err != nil {
		return Cursor{}, fmt.Errorf("cannot read fetch cursor: %w", err)
	}
	return c, nil
}

// AdvanceCursor 写入游标：UIDVALIDITY 与已有记录相同时 LastUID 只能增加或不变，否则返回 ErrCursorRegression 且不改动；
// UIDVALIDITY 不同或尚无记录时整体写入（重置）。调用方在一批邮件全部处理并提交后才调用。
// account、folder 与 UIDVALIDITY 在开始事务前校验。比较与写入由一条 UPSERT 完成：冲突时只有 WHERE 成立才更新，
// 受影响行数为 0 即是回退，因此多个进程并发写入同一游标也不会让它后退。
func (s *Store) AdvanceCursor(ctx context.Context, account, folder string, c Cursor) error {
	if err := checkMailbox(account, folder); err != nil {
		return fmt.Errorf("invalid fetch cursor: %w", err)
	}
	if c.UIDValidity == 0 {
		return errors.New("invalid fetch cursor: uid validity must be positive")
	}
	result, err := s.db.ExecContext(ctx,
		"INSERT INTO fetch_cursors (account, folder, uid_validity, last_uid, updated_at) VALUES (?, ?, ?, ?, ?) "+
			"ON CONFLICT (account, folder) DO UPDATE SET uid_validity = excluded.uid_validity, last_uid = excluded.last_uid, updated_at = excluded.updated_at "+
			"WHERE excluded.uid_validity <> fetch_cursors.uid_validity OR excluded.last_uid >= fetch_cursors.last_uid",
		account, folder, c.UIDValidity, c.LastUID, s.now().UnixMilli())
	if err != nil {
		return fmt.Errorf("cannot advance fetch cursor: %w", err)
	}
	written, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("cannot advance fetch cursor: %w", err)
	}
	if written == 0 {
		return ErrCursorRegression
	}
	return nil
}

// RecordRejection 写入被拒来信；同一 (account, folder, uid_validity, uid) 已有记录时返回原记录 ID 与 duplicate=true，不覆盖，
// 也不再检查任务。原因码不在列表中、字段长度或空白不符、含 NUL 或非法 UTF-8 时在开始事务前报错，错误文本不回显地址与 Message-ID；
// TaskID 不存在时返回 ErrNotFound。received_at 取存储时钟的当前时间。
func (s *Store) RecordRejection(ctx context.Context, r Rejection) (id int64, duplicate bool, err error) {
	if err := r.validate(); err != nil {
		return 0, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略；命中已有记录时由它结束只读的事务。
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, "SELECT id FROM inbound_rejections WHERE account = ? AND folder = ? AND uid_validity = ? AND uid = ?",
		r.Account, r.Folder, r.UIDValidity, r.UID).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("cannot read rejections: %w", err)
	}
	if r.TaskID != "" {
		if _, err := getTask(ctx, tx, r.TaskID); err != nil {
			return 0, false, err
		}
	}
	if err := tx.QueryRowContext(ctx,
		"INSERT INTO inbound_rejections (account, folder, uid_validity, uid, message_id, sender, reason, task_id, received_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id",
		r.Account, r.Folder, r.UIDValidity, r.UID, optional(r.MessageID), optional(r.Sender), string(r.Reason), optional(r.TaskID), s.now().UnixMilli()).
		Scan(&id); err != nil {
		return 0, false, fmt.Errorf("cannot record rejection: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("cannot commit rejection: %w", err)
	}
	return id, false, nil
}

// Rejections 按 (received_at, id) 升序返回 received_at 晚于 since 的被拒来信（按毫秒比较，恰在 since 的不返回），
// 至多 limit 条（1–1000），越界时在查询前报错。它只适合查看某一时刻之后的前 limit 条，不能用来逐页读完：
// 同一毫秒可能有多条记录，以上一页最后一条的 ReceivedAt 作为下一页的 since 会漏掉与它同一毫秒的其余记录。
// 原因码按原样返回，不按本版本的列表校验：它只用于展示，较新版本增补的原因码仍可读出。
func (s *Store) Rejections(ctx context.Context, since time.Time, limit int) ([]Rejection, error) {
	if limit < 1 || limit > maxRejectionsLimit {
		return nil, errors.New("invalid rejections query: limit must be 1-1000")
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, account, folder, uid_validity, uid, message_id, sender, reason, task_id, received_at FROM inbound_rejections "+
			"WHERE received_at > ? ORDER BY received_at, id LIMIT ?", since.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("cannot read rejections: %w", err)
	}
	defer rows.Close()
	var rejections []Rejection
	for rows.Next() {
		var r Rejection
		var messageID, sender, taskID sql.NullString
		var receivedAt int64
		if err := rows.Scan(&r.ID, &r.Account, &r.Folder, &r.UIDValidity, &r.UID, &messageID, &sender, &r.Reason, &taskID, &receivedAt); err != nil {
			return nil, fmt.Errorf("cannot read rejections: %w", err)
		}
		r.MessageID, r.Sender, r.TaskID = messageID.String, sender.String, taskID.String
		r.ReceivedAt = time.UnixMilli(receivedAt).UTC()
		rejections = append(rejections, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("cannot read rejections: %w", err)
	}
	return rejections, nil
}

// validate 在事务开始前检查原因码与各字段，规则与表约束一致；MessageID、Sender 与 TaskID 可为空，
// Sender 是规范化后的地址，不含空白。MessageID 与 Sender 须为合法 UTF-8 且不含 NUL，长度按 Unicode 字符计，
// 理由同 checkMailbox。错误文本不含地址与 Message-ID。
func (r Rejection) validate() error {
	if err := checkMailbox(r.Account, r.Folder); err != nil {
		return fmt.Errorf("invalid rejection: %w", err)
	}
	messageIDLen, senderLen := utf8.RuneCountInString(r.MessageID), utf8.RuneCountInString(r.Sender)
	switch {
	case !knownRejectReasons[r.Reason]:
		return fmt.Errorf("invalid rejection: unknown reason %q", r.Reason)
	case r.UIDValidity == 0 || r.UID == 0:
		return errors.New("invalid rejection: uid validity and uid must be positive")
	case r.TaskID != "" && !taskIDPattern.MatchString(r.TaskID):
		return errors.New("invalid rejection: task id must be empty or a 10-character task id")
	case r.MessageID != "" && (messageIDLen < 3 || messageIDLen > 998):
		return errors.New("invalid rejection: message id must be empty or 3-998 characters")
	case r.Sender != "" && (senderLen < 3 || senderLen > 254 || strings.ContainsFunc(r.Sender, unicode.IsSpace)):
		return errors.New("invalid rejection: sender must be empty or 3-254 characters without whitespace")
	case !utf8.ValidString(r.MessageID) || !utf8.ValidString(r.Sender) || strings.ContainsRune(r.MessageID, 0) || strings.ContainsRune(r.Sender, 0):
		return errors.New("invalid rejection: message id and sender must be valid UTF-8 without NUL")
	}
	return nil
}

// checkMailbox 检查账户与文件夹：账户为 3–254 个字符且不含空白，文件夹为 1–255 个字符、可以含空格，长度按 Unicode 字符计。
// 两者还须为合法 UTF-8 且不含 NUL：只有这样按字符计的长度才与表约束中 SQLite 的 length() 一致（length() 遇到 NUL 即停止计数，
// 遇到非法 UTF-8 时可能把多个字节计为一个字符），否则 Go 端判为合规的取值会在执行 SQL 后才被 CHECK 约束拒绝。
// 与 InboundReply.validate 相比只多出合法 UTF-8 的要求，其余规则相同。错误文本不含取值。
func checkMailbox(account, folder string) error {
	accountLen, folderLen := utf8.RuneCountInString(account), utf8.RuneCountInString(folder)
	switch {
	case accountLen < 3 || accountLen > 254 || strings.ContainsFunc(account, unicode.IsSpace):
		return errors.New("account must be 3-254 characters without whitespace")
	case folderLen < 1 || folderLen > 255:
		return errors.New("folder must be 1-255 characters")
	case !utf8.ValidString(account) || !utf8.ValidString(folder) || strings.ContainsRune(account, 0) || strings.ContainsRune(folder, 0):
		return errors.New("account and folder must be valid UTF-8 without NUL")
	}
	return nil
}

// optional 把空字符串写为 NULL，用于可为空的列。
func optional(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
