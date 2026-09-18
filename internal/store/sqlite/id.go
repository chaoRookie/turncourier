// Package sqlite 生成任务 ID：50 位随机数编码为 10 位 Crockford base32 小写字符。
package sqlite

import (
	"encoding/binary"
	"fmt"
	"io"
)

// crockfordAlphabet 是小写 Crockford base32 字母表，去掉了容易混淆的 i、l、o、u。
const crockfordAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// newTaskID 从 random 读取 7 字节，取前 50 位编码为 10 位 Crockford base32 小写字符。
// 用 io.ReadFull 读取，注入的随机源分多次返回数据时也能读满；不足 7 字节时返回错误。
func newTaskID(random io.Reader) (string, error) {
	var buf [8]byte
	if _, err := io.ReadFull(random, buf[:7]); err != nil {
		return "", fmt.Errorf("cannot generate task id: %w", err)
	}
	// 7 字节位于 64 位整数的高 56 位，右移 14 位后恰好剩下前 50 位。
	bits := binary.BigEndian.Uint64(buf[:]) >> 14
	id := make([]byte, 10)
	for i := len(id) - 1; i >= 0; i-- {
		id[i] = crockfordAlphabet[bits&31]
		bits >>= 5
	}
	return string(id), nil
}
