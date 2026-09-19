// Package sqlite 在打开数据库后与清理正文后尽力执行 TRUNCATE 检查点，清掉 WAL 中残留的历史帧（D5）。
package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
)

// truncateWAL 尽力执行 PRAGMA wal_checkpoint(TRUNCATE)，完成（结果行的 busy 为 0）时返回 true；出错或忙时返回 false，
// 不返回错误、不重试，由下一次清理或下一次打开再做。TRUNCATE 检查点会调用忙等待处理：在 busy_timeout 下，其他进程
// 只要持有读事务就会先等满期限，而存储只有一个连接，这段时间内同进程的全部操作都被阻塞。因此它取得这唯一的连接，
// 临时把 busy_timeout 设为 0，忙时立即返回，再恢复为 busyTimeoutMillis；恢复失败时让该连接作废，
// 下一次操作按 connectionParams 新建连接。调用方不得持有未结束的事务，否则取连接会一直等待。
func truncateWAL(ctx context.Context, db *sql.DB) bool {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	var busy, logFrames, checkpointed int
	_, err = conn.ExecContext(ctx, "PRAGMA busy_timeout = 0")
	if err == nil {
		err = conn.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed)
	}
	if _, restoreErr := conn.ExecContext(ctx, "PRAGMA busy_timeout = "+busyTimeoutMillis); restoreErr != nil {
		// 回调返回 driver.ErrBadConn 时 database/sql 关闭该连接，忙等待为 0 的连接不会回到连接池。
		conn.Raw(func(any) error { return driver.ErrBadConn })
		return false
	}
	return err == nil && busy == 0
}
