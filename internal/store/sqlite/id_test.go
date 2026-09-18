// Package sqlite 的任务 ID 测试用固定字节验证位序与字母表映射，并用真实随机源验证字符集。
package sqlite

import (
	"bytes"
	"crypto/rand"
	"regexp"
	"testing"
	"testing/iotest"
)

// TestNewTaskIDFixedInputs 用固定字节验证取前 50 位、按 5 位一组从高到低编码为 Crockford base32 小写字符。
// 除全 0 与全 1 外，用例覆盖字母表的全部 32 个符号，末字节低 6 位不参与编码。
func TestNewTaskIDFixedInputs(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  string
	}{
		{"全 0", []byte{0, 0, 0, 0, 0, 0, 0}, "0000000000"},
		{"全 1", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, "zzzzzzzzzz"},
		{"末 6 位被丢弃", []byte{0, 0, 0, 0, 0, 0, 0x3f}, "0000000000"},
		{"第 50 位", []byte{0, 0, 0, 0, 0, 0, 0x40}, "0000000001"},
		{"数字", []byte{0x00, 0x44, 0x32, 0x14, 0xc7, 0x42, 0x40}, "0123456789"},
		{"字母 a-k", []byte{0x52, 0xd8, 0xd7, 0x3e, 0x11, 0x94, 0xc0}, "abcdefghjk"},
		{"字母 m-x", []byte{0xa5, 0x6d, 0x7c, 0x67, 0x5b, 0xe7, 0x40}, "mnpqrstvwx"},
		{"字母 y-z", []byte{0xf7, 0xfd, 0xff, 0x7f, 0xdf, 0xf7, 0xc0}, "yzyzyzyzyz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newTaskID(bytes.NewReader(tt.input))
			if err != nil || got != tt.want {
				t.Errorf("newTaskID = %q, %v; want %q, nil", got, err, tt.want)
			}
		})
	}
}

// TestNewTaskIDReadsExactlySevenBytes 验证随机源分多次返回数据时仍能读满 7 字节，且不多读。
func TestNewTaskIDReadsExactlySevenBytes(t *testing.T) {
	input := bytes.NewReader([]byte{0x00, 0x44, 0x32, 0x14, 0xc7, 0x42, 0x40, 0xff})
	got, err := newTaskID(iotest.OneByteReader(input))
	if err != nil || got != "0123456789" {
		t.Errorf("newTaskID = %q, %v; want %q, nil", got, err, "0123456789")
	}
	if input.Len() != 1 {
		t.Errorf("剩余 %d 字节; want 1", input.Len())
	}
}

// TestNewTaskIDShortRead 验证随机源不足 7 字节时返回错误。
func TestNewTaskIDShortRead(t *testing.T) {
	for _, n := range []int{0, 6} {
		if got, err := newTaskID(bytes.NewReader(make([]byte, n))); err == nil {
			t.Errorf("%d 字节输入: newTaskID = %q, nil; want 错误", n, got)
		}
	}
}

// TestNewTaskIDRandom 验证 1000 次真实随机生成的 ID 都是 10 位小写 Crockford base32 字符。
func TestNewTaskIDRandom(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9abcdefghjkmnpqrstvwxyz]{10}$`)
	for range 1000 {
		id, err := newTaskID(rand.Reader)
		if err != nil {
			t.Fatalf("newTaskID 返回错误: %v", err)
		}
		if !pattern.MatchString(id) {
			t.Fatalf("newTaskID = %q; want 10 位小写 Crockford base32", id)
		}
	}
}
