// Package sqlite 的实例与密钥元数据测试用临时目录中的真实 SQLite 数据库验证实例 ID 的生成与并发创建、密钥的登记与查询。
package sqlite

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

// TestEnsureInstance 验证 EnsureInstance 从随机源读取 10 字节，编码为 16 位小写 Crockford base32 后写入：
// 10 个零字节得到 16 个 0，按 5 位分组依次为 0–15 的字节得到 0123456789abcdef，全 1 得到 16 个 z；
// 再次调用返回同一个值且 created 为 false，并且不再读取随机源；InstanceID 读到同一个值。
func TestEnsureInstance(t *testing.T) {
	tests := []struct {
		random []byte
		want   string
	}{
		{make([]byte, 10), "0000000000000000"},
		{[]byte{0x00, 0x44, 0x32, 0x14, 0xc7, 0x42, 0x54, 0xb6, 0x35, 0xcf}, "0123456789abcdef"},
		{bytes.Repeat([]byte{0xff}, 10), "zzzzzzzzzzzzzzzz"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			store, _ := openTaskStore(t, bytes.NewReader(tt.random))
			if id, err := store.InstanceID(t.Context()); !errors.Is(err, ErrNotFound) {
				t.Errorf("EnsureInstance 之前 InstanceID = %q, %v; want ErrNotFound", id, err)
			}
			id, created, err := store.EnsureInstance(t.Context())
			if err != nil || id != tt.want || !created {
				t.Fatalf("EnsureInstance = %q, %v, %v; want %q, true, nil", id, created, err, tt.want)
			}
			store.random = iotest.ErrReader(errors.New("random source must not be read again"))
			if id, created, err := store.EnsureInstance(t.Context()); err != nil || id != tt.want || created {
				t.Errorf("再次 EnsureInstance = %q, %v, %v; want %q, false, nil", id, created, err, tt.want)
			}
			if id, err := store.InstanceID(t.Context()); err != nil || id != tt.want {
				t.Errorf("InstanceID = %q, %v; want %q, nil", id, err, tt.want)
			}
		})
	}
}

// TestEnsureInstanceShortRandom 验证随机源不足 10 字节时 EnsureInstance 报错且不写入任何行。
func TestEnsureInstanceShortRandom(t *testing.T) {
	store, _ := openTaskStore(t, bytes.NewReader(make([]byte, 9)))
	if id, created, err := store.EnsureInstance(t.Context()); err == nil || id != "" || created {
		t.Errorf("EnsureInstance = %q, %v, %v; want 错误", id, created, err)
	}
	if got := countRows(t, store.db, "instance"); got != 0 {
		t.Errorf("instance 行数 = %d; want 0", got)
	}
	if id, err := store.InstanceID(t.Context()); !errors.Is(err, ErrNotFound) {
		t.Errorf("InstanceID = %q, %v; want ErrNotFound", id, err)
	}
}

// TestEnsureInstanceConcurrently 模拟两个进程同时首次运行：同一数据目录上两个独立 Open 的存储各在 10 个 goroutine 中
// 同时调用 EnsureInstance，全部返回同一个实例 ID，且恰有一次 created 为 true。
func TestEnsureInstanceConcurrently(t *testing.T) {
	dir := dataDir(t)
	stores := []*Store{openStore(t, dir), openStore(t, dir)}
	// result 是一次 EnsureInstance 调用的返回值。
	type result struct {
		id      string
		created bool
	}
	results := make(chan result, 20)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, store := range stores {
		for range 10 {
			wg.Go(func() {
				<-start
				id, created, err := store.EnsureInstance(t.Context())
				if err != nil {
					t.Errorf("EnsureInstance 返回错误: %v", err)
					return
				}
				results <- result{id, created}
			})
		}
	}
	close(start)
	wg.Wait()
	close(results)
	ids := make(map[string]int)
	created := 0
	for r := range results {
		ids[r.id]++
		if r.created {
			created++
		}
	}
	if len(ids) != 1 || created != 1 {
		t.Errorf("实例 ID = %v，created 为 true 的次数 = %d; want 20 次都是同一个 ID、恰有一次 created", ids, created)
	}
}

// TestRegisterKey 验证密钥元数据的登记与查询：登记后 active 的 kid、状态与校验值都能读回；
// 同一用途再次登记（同一或另一 kid）返回 ErrKeyExists 且校验值不变；未登记的 kid 与用途返回 ErrNotFound；
// 两种用途互不影响。
func TestRegisterKey(t *testing.T) {
	store := openStore(t, dataDir(t))
	ctx := t.Context()
	check := [8]byte{0x10, 0x21, 0x32, 0x43, 0x54, 0x65, 0x76, 0x87}
	other := [8]byte{0x87, 0x76, 0x65, 0x54, 0x43, 0x32, 0x21, 0x10}
	if err := store.RegisterKey(ctx, KeyPurposeToken, 1, check); err != nil {
		t.Fatalf("RegisterKey 返回错误: %v", err)
	}
	if kid, err := store.ActiveKeyID(ctx, KeyPurposeToken); err != nil || kid != 1 {
		t.Errorf("ActiveKeyID(token) = %d, %v; want 1, nil", kid, err)
	}
	if state, err := store.KeyStateOf(ctx, KeyPurposeToken, 1); err != nil || state != KeyActive {
		t.Errorf("KeyStateOf(token, 1) = %q, %v; want active, nil", state, err)
	}
	for _, kid := range []uint8{1, 2} {
		if err := store.RegisterKey(ctx, KeyPurposeToken, kid, other); !errors.Is(err, ErrKeyExists) {
			t.Errorf("再次登记 (token, %d): err = %v; want ErrKeyExists", kid, err)
		}
	}
	if got, err := store.KeyCheckOf(ctx, KeyPurposeToken, 1); err != nil || got != check {
		t.Errorf("KeyCheckOf(token, 1) = %x, %v; want 登记时的校验值", got, err)
	}
	if state, err := store.KeyStateOf(ctx, KeyPurposeToken, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("KeyStateOf(token, 2) = %q, %v; want ErrNotFound", state, err)
	}
	if got, err := store.KeyCheckOf(ctx, KeyPurposeToken, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("KeyCheckOf(token, 2) = %x, %v; want ErrNotFound", got, err)
	}
	if kid, err := store.ActiveKeyID(ctx, KeyPurposePayload); !errors.Is(err, ErrNotFound) {
		t.Errorf("ActiveKeyID(payload) = %d, %v; want ErrNotFound", kid, err)
	}
	if got := countRows(t, store.db, "crypto_keys"); got != 1 {
		t.Errorf("crypto_keys 行数 = %d; want 1", got)
	}

	if err := store.RegisterKey(ctx, KeyPurposePayload, 7, other); err != nil {
		t.Fatalf("RegisterKey(payload, 7) 返回错误: %v", err)
	}
	if kid, err := store.ActiveKeyID(ctx, KeyPurposePayload); err != nil || kid != 7 {
		t.Errorf("ActiveKeyID(payload) = %d, %v; want 7, nil", kid, err)
	}
	if got, err := store.KeyCheckOf(ctx, KeyPurposePayload, 7); err != nil || got != other {
		t.Errorf("KeyCheckOf(payload, 7) = %x, %v; want 登记时的校验值", got, err)
	}
	if got, err := store.KeyCheckOf(ctx, KeyPurposeToken, 1); err != nil || got != check {
		t.Errorf("登记 payload 后 KeyCheckOf(token, 1) = %x, %v; want 不变", got, err)
	}
}

// TestRegisterKeyNonActive 验证非 active 的密钥：以绑定参数直接插入一条 retired 或 destroyed 的 payload 密钥后，
// ActiveKeyID(payload) 返回 ErrNotFound，KeyStateOf 读回该状态；该用途已有密钥，RegisterKey(payload, 2) 仍返回
// ErrKeyExists，crypto_keys 行数不变。
func TestRegisterKeyNonActive(t *testing.T) {
	for _, state := range []KeyState{KeyRetired, KeyDestroyed} {
		t.Run(string(state), func(t *testing.T) {
			store := openStore(t, dataDir(t))
			ctx := t.Context()
			if _, err := store.db.ExecContext(ctx,
				"INSERT INTO crypto_keys (purpose, kid, state, key_check, created_at, updated_at) VALUES (?, 1, ?, ?, 0, 0)",
				string(KeyPurposePayload), string(state), bytes.Repeat([]byte{0x5a}, 8)); err != nil {
				t.Fatalf("插入 %s 密钥失败: %v", state, err)
			}
			if kid, err := store.ActiveKeyID(ctx, KeyPurposePayload); !errors.Is(err, ErrNotFound) {
				t.Errorf("ActiveKeyID(payload) = %d, %v; want ErrNotFound", kid, err)
			}
			if got, err := store.KeyStateOf(ctx, KeyPurposePayload, 1); err != nil || got != state {
				t.Errorf("KeyStateOf(payload, 1) = %q, %v; want %q, nil", got, err, state)
			}
			if err := store.RegisterKey(ctx, KeyPurposePayload, 2, [8]byte{}); !errors.Is(err, ErrKeyExists) {
				t.Errorf("RegisterKey(payload, 2): err = %v; want ErrKeyExists", err)
			}
			if got := countRows(t, store.db, "crypto_keys"); got != 1 {
				t.Errorf("crypto_keys 行数 = %d; want 1", got)
			}
		})
	}
}

// TestRegisterKeyValidation 验证 kid 为 0 或用途未知时，RegisterKey 在开始事务前返回包装 ErrInvalidArgument 的错误且不写入：
// 上下文已取消时仍返回参数错误，而不是 context.Canceled。
func TestRegisterKeyValidation(t *testing.T) {
	store := openStore(t, dataDir(t))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	check := [8]byte{0x10, 0x21, 0x32, 0x43, 0x54, 0x65, 0x76, 0x87}
	for _, tt := range []struct {
		name    string
		purpose KeyPurpose
		kid     uint8
	}{
		{"kid 为 0", KeyPurposeToken, 0},
		{"未知用途", KeyPurpose("bogus"), 1},
		{"大写用途", KeyPurpose("TOKEN"), 1},
	} {
		err := store.RegisterKey(ctx, tt.purpose, tt.kid, check)
		if err == nil || !strings.Contains(err.Error(), "invalid key registration") || !errors.Is(err, ErrInvalidArgument) || errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v; want 包装 ErrInvalidArgument、含 \"invalid key registration\" 的错误而不是 context.Canceled", tt.name, err)
		}
	}
	if got := countRows(t, store.db, "crypto_keys"); got != 0 {
		t.Errorf("crypto_keys 行数 = %d; want 0", got)
	}
}
