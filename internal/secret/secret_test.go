package secret

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

const canary = "sk-ant-api03-AAAAbbbbCCCCddddEEEEffffGGGGhhhh1111"

func TestKeyFormattingAlwaysRedactsPlaintext(t *testing.T) {
	key, err := New("formatted", canary)
	if err != nil {
		t.Fatal(err)
	}
	for _, formatted := range []string{
		fmt.Sprintf("%v", key),
		fmt.Sprintf("%+v", key),
		fmt.Sprintf("%#v", key),
		fmt.Sprintf("%s", key),
		fmt.Sprintf("%q", key),
	} {
		if strings.Contains(formatted, canary) {
			t.Fatalf("fmt 泄漏明文 key:%s", formatted)
		}
		if !strings.Contains(formatted, "formatted") || !strings.Contains(formatted, "1111") {
			t.Fatalf("fmt 应输出名称+末4位:%s", formatted)
		}
	}
}

// 脱敏渲染:名称+末4位;完整明文绝不出现在渲染结果中
func TestMaskedRendersRedacted(t *testing.T) {
	k, err := New("work", canary)
	if err != nil {
		t.Fatal(err)
	}
	ref := k.Ref()
	got := ref.Masked()
	if strings.Contains(got, canary) {
		t.Errorf("脱敏渲染泄漏完整明文: %s", got)
	}
	if !strings.Contains(got, "work") {
		t.Errorf("脱敏渲染应含名称: %s", got)
	}
	if !strings.Contains(got, canary[len(canary)-4:]) {
		t.Errorf("脱敏渲染应含末4位: %s", got)
	}
	// 除末4位外,明文的其它片段不得出现
	if strings.Contains(got, canary[:len(canary)-4]) {
		t.Errorf("脱敏渲染泄漏明文前缀: %s", got)
	}
}

// 默认命名与空 key 拒绝
func TestNewDefaultsAndRejects(t *testing.T) {
	k, err := New("", canary)
	if err != nil {
		t.Fatal(err)
	}
	if k.Ref().Name != "default" {
		t.Errorf("空名称应默认 default,得到 %s", k.Ref().Name)
	}
	if _, err := New("x", ""); err == nil {
		t.Error("空明文必须拒绝")
	}
	if _, err := New("x", "1234"); err == nil {
		t.Error("不超过 4 字节的 key 无法只显示末4位,必须拒绝")
	}
	malformed := Ref{Name: "external", Last4: canary, Len: len(canary)}.Masked()
	if strings.Contains(malformed, canary) || !strings.Contains(malformed, "1111") {
		t.Fatalf("外部构造 Ref 也必须强制截到末4位:%s", malformed)
	}
}

// Store 只进不出:Refs 只暴露脱敏引用
func TestStoreOnlyExposesRefs(t *testing.T) {
	s := NewStore()
	for _, name := range []string{"a", "b", "c"} { // 多 key ≥3
		k, err := New(name, canary+name)
		if err != nil {
			t.Fatal(err)
		}
		s.Add(k)
	}
	if s.Len() != 3 {
		t.Fatalf("Store 应有 3 枚 key,得到 %d", s.Len())
	}
	for _, r := range s.Refs() {
		if strings.Contains(r.Masked(), canary) {
			t.Errorf("Store 泄漏明文: %s", r.Masked())
		}
	}
}

func TestReadStdinReadsOneLineWithoutLeakingPlaintext(t *testing.T) {
	input := "  " + canary + "  \r\nnext-key\n"
	key, err := ReadStdin("stdin", strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	ref := key.Ref()
	if ref.Name != "stdin" || ref.Len != len("  "+canary+"  ") {
		t.Fatalf("stdin key reference is wrong: %#v", ref)
	}
	if strings.Contains(ref.Masked(), canary) {
		t.Fatalf("stdin key leaked through reference: %s", ref.Masked())
	}
}

func TestReadStdinAcceptsFinalLineWithoutNewline(t *testing.T) {
	key, err := ReadStdin("stdin", strings.NewReader(canary))
	if err != nil {
		t.Fatal(err)
	}
	if key.Ref().Len != len(canary) {
		t.Fatalf("unexpected key length: %d", key.Ref().Len)
	}
}

func TestReadStdinRejectsEmptyAndReadFailure(t *testing.T) {
	if _, err := ReadStdin("stdin", strings.NewReader("\n")); err == nil {
		t.Fatal("empty stdin key must fail")
	}
	if _, err := ReadStdin("stdin", failingReader{}); err == nil {
		t.Fatal("stdin read failure must be returned")
	}
}

func TestReadTTYDisablesAndRestoresEcho(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "tty-input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := input.WriteString(canary + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}

	echo := &fakeEchoController{}
	key, err := readTTY("tty", input, echo)
	if err != nil {
		t.Fatal(err)
	}
	if echo.disabled != 1 || echo.restored != 1 {
		t.Fatalf("echo disable/restore = %d/%d, want 1/1", echo.disabled, echo.restored)
	}
	if key.Ref().Len != len(canary) {
		t.Fatalf("unexpected key length: %d", key.Ref().Len)
	}
}

func TestReadTTYRestoresEchoAfterReadFailure(t *testing.T) {
	input, err := os.CreateTemp(t.TempDir(), "empty-tty-input")
	if err != nil {
		t.Fatal(err)
	}
	echo := &fakeEchoController{}
	if _, err := readTTY("tty", input, echo); err == nil {
		t.Fatal("empty TTY key must fail")
	}
	if echo.disabled != 1 || echo.restored != 1 {
		t.Fatalf("echo disable/restore = %d/%d, want 1/1", echo.disabled, echo.restored)
	}
}

type fakeEchoController struct {
	disabled int
	restored int
}

func (f *fakeEchoController) DisableEcho(*os.File) (func() error, error) {
	f.disabled++
	return func() error {
		f.restored++
		return nil
	}, nil
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read failure")
}
