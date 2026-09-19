// Package sqlite 保存实例 ID 与密钥元数据；密钥材料只在 Keychain 中，表中只有用途、kid、状态与校验值。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
)

// KeyPurpose 是 crypto_keys 中密钥的用途。
type KeyPurpose string

const (
	KeyPurposeToken   KeyPurpose = "token"
	KeyPurposePayload KeyPurpose = "payload"
)

// KeyState 是密钥的生命周期状态；4a 只登记 active，retired 与 destroyed 留给以后的轮换命令。
type KeyState string

const (
	KeyActive    KeyState = "active"
	KeyRetired   KeyState = "retired"
	KeyDestroyed KeyState = "destroyed"
)

// ErrKeyExists 表示该用途已登记过密钥（4a 每个用途只登记 kid=1）。
var ErrKeyExists = errors.New("key already registered")

// instanceEncoding 把 10 字节随机数编码为 16 位小写 Crockford base32，与任务 ID 同一字母表。
var instanceEncoding = base32.NewEncoding(crockfordAlphabet).WithPadding(base32.NoPadding)

// EnsureInstance 返回实例 ID；不存在时从随机源读取 10 字节，编码为 16 位小写 Crockford base32 后写入，created 为 true。
// 在一个 IMMEDIATE 事务中先读后写，多个进程同时调用时只有一个写入，其余读到同一个值；已存在时不读取随机源。
func (s *Store) EnsureInstance(ctx context.Context) (id string, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略；已存在时由它结束只读的事务。
	defer tx.Rollback()
	id, err = instanceID(ctx, tx)
	if !errors.Is(err, ErrNotFound) {
		return id, false, err
	}
	var raw [10]byte
	if _, err := io.ReadFull(s.random, raw[:]); err != nil {
		return "", false, fmt.Errorf("cannot generate instance id: %w", err)
	}
	id = instanceEncoding.EncodeToString(raw[:])
	if _, err := tx.ExecContext(ctx, "INSERT INTO instance (singleton, instance_id, created_at) VALUES (1, ?, ?)", id, s.now().UnixMilli()); err != nil {
		return "", false, fmt.Errorf("cannot record instance id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("cannot commit instance id: %w", err)
	}
	return id, true, nil
}

// InstanceID 读取实例 ID；尚未运行 init 时返回 ErrNotFound。
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	return instanceID(ctx, s.db)
}

// instanceID 通过 q 读取实例 ID，使读取在事务内外都能复用；不存在时返回 ErrNotFound。
func instanceID(ctx context.Context, q rowQuerier) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, "SELECT instance_id FROM instance WHERE singleton = 1").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("instance id: %w", ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("cannot read instance id: %w", err)
	}
	return id, nil
}

// RegisterKey 把 (purpose, kid) 登记为 active，并保存 keychain.KeyCheck 算出的校验值；kid 须为 1–255。
// 该用途已有任何密钥时返回 ErrKeyExists。调用方必须先把密钥写入 Keychain 并读回核对，再登记。
// 元数据只说明该密钥曾被登记；使用前须读出 Keychain 中的密钥并与 KeyCheckOf 比对，不符即拒绝使用。
// 用途未知或 kid 为 0 时在开始事务前报错。
func (s *Store) RegisterKey(ctx context.Context, purpose KeyPurpose, kid uint8, check [8]byte) error {
	if purpose != KeyPurposeToken && purpose != KeyPurposePayload {
		return fmt.Errorf("invalid key registration: unknown purpose %q", purpose)
	}
	if kid == 0 {
		return errors.New("invalid key registration: key id must be 1-255")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("cannot begin transaction: %w", err)
	}
	// 提交成功后 Rollback 只返回 sql.ErrTxDone，可以安全忽略。
	defer tx.Rollback()
	var registered int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM crypto_keys WHERE purpose = ?", string(purpose)).Scan(&registered); err != nil {
		return fmt.Errorf("cannot read key metadata: %w", err)
	}
	if registered > 0 {
		return fmt.Errorf("%w: purpose %s", ErrKeyExists, purpose)
	}
	now := s.now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO crypto_keys (purpose, kid, state, key_check, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)",
		string(purpose), kid, string(KeyActive), check[:], now, now); err != nil {
		return fmt.Errorf("cannot register key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("cannot commit key registration: %w", err)
	}
	return nil
}

// KeyCheckOf 返回 (purpose, kid) 登记的校验值；未登记时返回 ErrNotFound。
func (s *Store) KeyCheckOf(ctx context.Context, purpose KeyPurpose, kid uint8) ([8]byte, error) {
	var check [8]byte
	var stored []byte
	err := s.db.QueryRowContext(ctx, "SELECT key_check FROM crypto_keys WHERE purpose = ? AND kid = ?", string(purpose), kid).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return check, fmt.Errorf("key %s/%d: %w", purpose, kid, ErrNotFound)
	}
	if err != nil {
		return check, fmt.Errorf("cannot read key metadata: %w", err)
	}
	// 表约束保证恰为 8 字节；即使被外部工具改坏，copy 也只会得到不相符的校验值，使调用方拒绝使用该密钥。
	copy(check[:], stored)
	return check, nil
}

// ActiveKeyID 返回该用途 active 密钥的 kid；没有时返回 ErrNotFound。
func (s *Store) ActiveKeyID(ctx context.Context, purpose KeyPurpose) (uint8, error) {
	var kid uint8
	err := s.db.QueryRowContext(ctx, "SELECT kid FROM crypto_keys WHERE purpose = ? AND state = ?", string(purpose), string(KeyActive)).Scan(&kid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("active %s key: %w", purpose, ErrNotFound)
	}
	if err != nil {
		return 0, fmt.Errorf("cannot read key metadata: %w", err)
	}
	return kid, nil
}

// KeyStateOf 返回 (purpose, kid) 的状态；未登记时返回 ErrNotFound。
func (s *Store) KeyStateOf(ctx context.Context, purpose KeyPurpose, kid uint8) (KeyState, error) {
	var state KeyState
	err := s.db.QueryRowContext(ctx, "SELECT state FROM crypto_keys WHERE purpose = ? AND kid = ?", string(purpose), kid).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("key %s/%d: %w", purpose, kid, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("cannot read key metadata: %w", err)
	}
	return state, nil
}
