// Package cli 的交互终端基于 x/term：提示写到输出终端，逐字节回显读入一行，或关闭回显后读入机密；
// 读取在后台协程中进行，ctx 结束（Ctrl-C）时恢复终端回显并立即返回 ctx.Err()。
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"
)

// maxLineBytes 是 ReadLine 一行的上限，不含结尾换行。
const maxLineBytes = 1024

// errLineTooLong 表示输入的一行超过 maxLineBytes；错误不回显输入。
var errLineTooLong = errors.New("input line is longer than 1024 bytes")

// fileTerminal 是基于标准输入与标准输出文件的 Terminal 生产实现。
type fileTerminal struct {
	in, out *os.File
}

// NewTerminal 返回基于 x/term 的生产实现；提示写到 out。
func NewTerminal(in, out *os.File) Terminal {
	return &fileTerminal{in: in, out: out}
}

// Interactive 报告标准输入与标准输出是否都是终端。
func (t *fileTerminal) Interactive() bool {
	return term.IsTerminal(int(t.in.Fd())) && term.IsTerminal(int(t.out.Fd()))
}

// ReadLine 写出提示后回显读入一行，去掉结尾换行；至多 1024 字节，超过时报错。
// 逐字节读取、不经带缓冲的读取器，这一行之后的输入留给随后的 ReadSecret。
func (t *fileTerminal) ReadLine(ctx context.Context, prompt string) (string, error) {
	if _, err := io.WriteString(t.out, prompt); err != nil {
		return "", err
	}
	return await(ctx, func() (string, error) { return readLine(t.in) })
}

// ReadSecret 写出提示后关闭回显读入一行，与 ReadLine 一样逐字节读取、至多 1024 字节；标准输入不是终端时返回错误且不读取。
// 回显在启动读取协程之前同步关闭，返回前（包括 ctx 先结束时）用事先保存的状态恢复，恢复因此一定发生在关闭之后。
// 不在协程中调用 term.ReadPassword：它在协程里才关闭回显，ctx 恰在此前结束时，恢复会先于关闭发生，进程退出后终端停留在不回显的状态。
// 被放弃的读取协程只读取、不改动终端设置，在进程退出前一直阻塞。
func (t *fileTerminal) ReadSecret(ctx context.Context, prompt string) (string, error) {
	fd := int(t.in.Fd())
	state, err := term.GetState(fd)
	if err != nil {
		return "", fmt.Errorf("cannot read a secret: %w", err)
	}
	// ctx 已结束时不关闭回显、不写提示，也不启动读取。
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := disableEcho(fd); err != nil {
		return "", fmt.Errorf("cannot read a secret: %w", err)
	}
	defer term.Restore(fd, state)
	if _, err := io.WriteString(t.out, prompt); err != nil {
		return "", err
	}
	secret, err := await(ctx, func() (string, error) { return readLine(t.in) })
	// 回显关闭时终端不显示用户按下的换行，补一个换行使下一行输出另起一行。
	io.WriteString(t.out, "\n")
	return secret, err
}

// await 在新协程中执行阻塞的 read，ctx 先结束时立即返回 ctx.Err()，read 的结果被丢弃。
func await(ctx context.Context, read func() (string, error)) (string, error) {
	// result 是一次读取的结果，经带缓冲的通道交回，读取被放弃时协程也不会阻塞在发送上。
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		text, err := read()
		done <- result{text, err}
	}()
	select {
	case r := <-done:
		return r.text, r.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// readLine 从 r 逐字节读到换行，返回不含换行的内容；超过 maxLineBytes 时在下一个字节处停止并报错，
// 换行之前遇到输入结束时返回 io.EOF。
func readLine(r io.Reader) (string, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return string(line), nil
			}
			if len(line) == maxLineBytes {
				return "", errLineTooLong
			}
			line = append(line, b[0])
			continue
		}
		if err != nil {
			return "", err
		}
	}
}
