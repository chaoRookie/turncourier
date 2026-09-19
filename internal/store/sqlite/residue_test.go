// Package sqlite 的残留测试直接读取数据目录中的数据库文件与 WAL 文件，验证通知内容进入终态、执行检查点之后磁盘上不留密文；
// 两个对照组在另一目录去掉 secure_delete 或改用 FAST，证明这项检查能发现残留，并能区分 ON 与 FAST（D5）。
package sqlite

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/chaoRookie/turncourier/internal/security/payload"
)

// fragmentLen 是在磁盘上查找的密文片段长度。
const fragmentLen = 32

// residueSizes 是残留测试的两种内容大小：64 字节时密文位于页内单元格，64 KiB 时占用溢出页链（真实通知超过约 4 KB 即是如此）。
var residueSizes = []int{64, 64 << 10}

// readDataFiles 读取数据目录中的数据库文件与 -wal 文件；WAL 文件不存在时返回空内容。
func readDataFiles(t *testing.T, dataDir string) (db, wal []byte) {
	t.Helper()
	db, err := os.ReadFile(filepath.Join(dataDir, databaseFileName))
	if err != nil {
		t.Fatalf("读取数据库文件失败: %v", err)
	}
	wal, err = os.ReadFile(filepath.Join(dataDir, databaseFileName+"-wal"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("读取 WAL 文件失败: %v", err)
	}
	return db, wal
}

// assertAbsentOnDisk 读取数据目录中的数据库文件与 -wal 文件，断言都不含 needle：needle 不短于 32 字节时，它逐字节滑动得到的
// 任何一个 32 字节片段都不能出现，短于 32 字节时按整体查找。可一次传入多个 needle，文件只读取一次。
func assertAbsentOnDisk(t *testing.T, dataDir string, needles ...[]byte) {
	t.Helper()
	db, wal := readDataFiles(t, dataDir)
	for i, needle := range needles {
		if found := fragmentsIn(db, needle) + fragmentsIn(wal, needle); found > 0 {
			t.Fatalf("第 %d 个 needle（%d 字节）有 %d 个片段仍在数据库文件或 WAL 文件中", i, len(needle), found)
		}
	}
}

// fragmentsIn 把 needle 逐字节滑动得到的全部 32 字节片段（needle 短于 32 字节时只有它本身）放进集合，再逐字节扫描 data 查表，
// 返回出现在 data 中的不同片段个数。64 KiB 密文约 6.5 万个片段，扫描数百 KB 的文件只需数毫秒。
func fragmentsIn(data, needle []byte) int {
	size := min(fragmentLen, len(needle))
	fragments := make(map[string]bool, len(needle)-size+1)
	for i := 0; i+size <= len(needle); i++ {
		fragments[string(needle[i:i+size])] = false
	}
	found := 0
	for i := 0; i+size <= len(data); i++ {
		if seen, ok := fragments[string(data[i:i+size])]; ok && !seen {
			fragments[string(data[i:i+size])] = true
			found++
		}
	}
	return found
}

// TestFragmentsIn 验证残留检查逐字节滑动：从 needle 任意位置开始、不短于 32 字节的残留都能发现，片段数为残留长度减 31；
// 31 字节的残留不算命中；短于 32 字节的 needle 按整体查找。
func TestFragmentsIn(t *testing.T) {
	// 37 与 256 互素，前 93 个字节互不相同，也都不是填充字节 0xff，所有片段互不相同。
	needle := make([]byte, 93)
	for i := range needle {
		needle[i] = byte(i*37 + 11)
	}
	padding := bytes.Repeat([]byte{0xff}, 40)
	for _, tt := range []struct{ start, end, want int }{{10, 42, 1}, {10, 60, 19}, {61, 93, 1}, {10, 41, 0}} {
		data := slices.Concat(padding, needle[tt.start:tt.end], padding)
		if got := fragmentsIn(data, needle); got != tt.want {
			t.Errorf("残留 needle[%d:%d]: fragmentsIn = %d; want %d", tt.start, tt.end, got, tt.want)
		}
	}
	if got := fragmentsIn([]byte("xx"+notificationCanary+"xx"), []byte(notificationCanary)); got != 1 {
		t.Errorf("短 needle 整体出现: fragmentsIn = %d; want 1", got)
	}
	if got := fragmentsIn([]byte(notificationCanary[1:]), []byte(notificationCanary)); got != 0 {
		t.Errorf("短 needle 只出现一部分: fragmentsIn = %d; want 0", got)
	}
}

// sealAndSend 为任务创建内容为 size 字节的通知，读出其密文，再领取并标记 SENT，返回密文；内容进入 SENT 时由触发器删除，
// MarkNotificationSent 提交后执行 TRUNCATE 检查点。
func sealAndSend(t *testing.T, s *Store, taskID string, size int) []byte {
	t.Helper()
	in := notificationFor(taskID, "residue")
	in.Content = bytes.Repeat([]byte("residue canary "), size/15+1)[:size]
	created := mustCreateNotification(t, s, in)
	var sealed []byte
	if err := s.db.QueryRowContext(t.Context(), "SELECT sealed FROM notification_payloads WHERE notification_id = ?", created.ID).Scan(&sealed); err != nil {
		t.Fatalf("读取密文失败: %v", err)
	}
	if claimed, content := mustClaimNotification(t, s); claimed.ID != created.ID || !bytes.Equal(content, in.Content) {
		t.Fatalf("领取 = 通知 %d; want %d 与原内容", claimed.ID, created.ID)
	}
	must(t, "MarkNotificationSent")(s.MarkNotificationSent(t.Context(), created.ID))
	if n := payloadCount(t, s, created.ID); n != 0 {
		t.Fatalf("SENT 后正文行数 = %d; want 0", n)
	}
	return sealed
}

// TestNotificationPayloadResidue 验证经 Open 打开的存储（secure_delete 为 ON）在通知内容进入 SENT、执行检查点之后，
// 数据库文件与 WAL 文件中都找不到密文的任何 32 字节片段；64 字节与 64 KiB 两种大小各跑一遍。
func TestNotificationPayloadResidue(t *testing.T) {
	for _, size := range residueSizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			store, _, running := newNotificationStore(t)
			sealed := sealAndSend(t, store, running.ID, size)
			if len(sealed) != size+payload.Overhead {
				t.Fatalf("密文长度 = %d; want %d", len(sealed), size+payload.Overhead)
			}
			assertAbsentOnDisk(t, storeDir(t, store), sealed)
		})
	}
}

// openControlStore 在新目录中用测试内拼接的连接串直接打开数据库（不经 Open）：secureDelete 为空时不设置 secure_delete，
// 否则追加 _pragma=secure_delete(secureDelete)；执行迁移后构造使用同一时钟、随机源与测试正文密钥的存储，登记测试密钥并启动任务。
// 返回存储、数据目录与 RUNNING 任务。
func openControlStore(t *testing.T, secureDelete string) (*Store, string, Task) {
	t.Helper()
	dir := t.TempDir()
	dsn := "file:" + uriEscaper.Replace(filepath.Join(dir, databaseFileName)) +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)&_txlock=immediate"
	if secureDelete != "" {
		dsn += "&_pragma=secure_delete(" + secureDelete + ")"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("打开对照数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	if err := migrate(t.Context(), db, migrationFS); err != nil {
		t.Fatalf("迁移对照数据库失败: %v", err)
	}
	clock := testClock()
	store := &Store{db: db, now: func() time.Time { return *clock }, random: rand.NewChaCha8([32]byte{}), payloadKey: newTestPayloadKey(t, 1)}
	registerTestKeys(t, store)
	return store, dir, startTask(t, store)
}

// TestNotificationPayloadResidueControls 是残留测试的两个对照组：在另一目录用测试内拼接的连接串直接打开数据库，执行同样的插入、
// 删除与 TRUNCATE 检查点。去掉 secure_delete 时两种大小都能在数据库文件中找到密文片段；改用 FAST 时 64 KiB 内容的密文片段仍能找到
// （FAST 对溢出页无效），证明测试能区分 ON 与 FAST。各组找到的片段数以 t.Logf 输出，供实施说明记录。
func TestNotificationPayloadResidueControls(t *testing.T) {
	modes := []struct {
		name, pragma, want string
		mustFind           func(size int) bool
	}{
		{"关闭", "", "0", func(int) bool { return true }},
		{"FAST", "FAST", "2", func(size int) bool { return size > 4096 }},
	}
	for _, mode := range modes {
		for _, size := range residueSizes {
			t.Run(fmt.Sprintf("%s/%d", mode.name, size), func(t *testing.T) {
				store, dir, running := openControlStore(t, mode.pragma)
				var got string
				if err := store.db.QueryRowContext(t.Context(), "PRAGMA secure_delete").Scan(&got); err != nil || got != mode.want {
					t.Fatalf("PRAGMA secure_delete = %q, %v; want %s", got, err, mode.want)
				}
				sealed := sealAndSend(t, store, running.ID, size)
				db, wal := readDataFiles(t, dir)
				inDB := fragmentsIn(db, sealed)
				t.Logf("secure_delete %s、内容 %d 字节：数据库文件含 %d/%d 个密文片段，WAL %d 字节", mode.name, size, inDB, len(sealed)-fragmentLen+1, len(wal))
				if mode.mustFind(size) && inDB == 0 {
					t.Errorf("对照组应能在数据库文件中找到密文片段，实际一个也没有")
				}
			})
		}
	}
}
