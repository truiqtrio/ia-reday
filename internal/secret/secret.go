// Package secret 唯一持有明文 key 的包(硬约束)。
// 边界:明文只进不出——本包不导出任何返回完整明文的函数,包外只流通 Ref(名称+末4位)。
// argv/env/日志/journal/一切输出零明文;真实调用所需明文的使用通道在实施期设计,且不出包。
package secret

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Key 命名密钥。字段不导出:包外无法接触明文。
type Key struct {
	name  string
	value string // 明文,禁止出包
}

// Format forces every fmt verb through the redacted reference. This protects
// ordinary error paths such as fmt.Printf("%+v", key) while Reveal remains the
// only explicit plaintext access contract.
func (k Key) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, k.Ref().Masked())
}

// New 构造命名 key;name 为空时默认 "default",空明文直接拒绝
func New(name, value string) (Key, error) {
	if value == "" {
		return Key{}, errors.New("secret: 空密钥")
	}
	if len(value) <= 4 {
		return Key{}, errors.New("secret: 密钥必须长于 4 字节,否则无法安全脱敏")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	return Key{name: name, value: value}, nil
}

// Ref 脱敏引用:包外流通的唯一形态(名称+末4位+长度)
type Ref struct {
	Name  string
	Last4 string
	Len   int
}

// Ref 生成脱敏引用。New 拒绝不足 5 字节的值,因此末 4 位永远
// 不会等于完整密钥。
func (k Key) Ref() Ref {
	last4 := k.value
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	return Ref{Name: k.name, Last4: last4, Len: len(k.value)}
}

// Reveal 受控通道:本包唯一放行的明文出口。
// fn 以明文为参数被同步调用;调用方契约(硬约束):
// 明文只许进入许可的写入物料(0600 暂存),禁止入日志/journal/argv/env/任何输出。
func (k Key) Reveal(fn func(plaintext string)) {
	fn(k.value)
}

// Masked 统一脱敏渲染:名称+末4位,如 `default(…wxyz, 67 chars)`;完整 key 永不出现
func (r Ref) Masked() string {
	last4 := r.Last4
	if len(last4) > 4 {
		last4 = last4[len(last4)-4:]
	}
	return fmt.Sprintf("%s(…%s, %d chars)", r.Name, last4, r.Len)
}

// Store 命名 key 清单:内存只进不出。
// 刻意不提供按名取明文的方法——包外拿不到明文;
// 实施期如需向 probe/adapter 供 key,只能在本包内部以受控回调完成,且不得外泄。
type Store struct {
	keys map[string]Key
}

func NewStore() *Store {
	return &Store{keys: make(map[string]Key)}
}

// Add 加入命名 key(多 key ≥3 的场景在此累积)
func (s *Store) Add(k Key) {
	s.keys[k.name] = k
}

// Refs 全部 key 的脱敏引用,供 UI 清单展示
func (s *Store) Refs() []Ref {
	out := make([]Ref, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k.Ref())
	}
	return out
}

// Len key 数量
func (s *Store) Len() int { return len(s.keys) }

// echoController makes terminal state changes injectable for tests without
// exposing an alternate plaintext-handling API.
type echoController interface {
	DisableEcho(*os.File) (restore func() error, err error)
}

type platformEchoController struct{}

func (platformEchoController) DisableEcho(input *os.File) (func() error, error) {
	return disableEcho(input)
}

// ReadTTY reads one key line with terminal echo disabled. The key remains
// encapsulated in Key; this function never writes it to an output stream.
func ReadTTY(name string, input *os.File) (Key, error) {
	return readTTY(name, input, platformEchoController{})
}

// ReadStdin reads one newline-delimited key from r, for the --key-stdin path.
// It intentionally accepts a final line without a newline, which is common in
// pipelines. Only the line ending is removed; whitespace is part of a key.
func ReadStdin(name string, r io.Reader) (Key, error) {
	return readKeyLine(name, r)
}

func readTTY(name string, input *os.File, echo echoController) (key Key, err error) {
	if input == nil {
		return Key{}, errors.New("secret: TTY 输入为空")
	}
	if echo == nil {
		return Key{}, errors.New("secret: echo 控制器为空")
	}

	restore, err := echo.DisableEcho(input)
	if err != nil {
		return Key{}, fmt.Errorf("secret: 禁用 TTY 回显: %w", err)
	}
	if restore == nil {
		return Key{}, errors.New("secret: echo 控制器未提供恢复函数")
	}

	key, readErr := readKeyLine(name, input)
	restoreErr := restore()
	if readErr != nil || restoreErr != nil {
		return Key{}, errors.Join(readErr, restoreErr)
	}
	return key, nil
}

func readKeyLine(name string, r io.Reader) (Key, error) {
	if r == nil {
		return Key{}, errors.New("secret: 输入为空")
	}

	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return Key{}, fmt.Errorf("secret: 读取密钥: %w", err)
	}
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	return New(name, line)
}
