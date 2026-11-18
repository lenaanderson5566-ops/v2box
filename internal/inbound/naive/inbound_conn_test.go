package naive

import (
	"bytes"
	"io"
	"testing"
)

func TestPaddingLastFrameCannotBeUnwrappedEarly(t *testing.T) {
	// Seven frames were already read. The eighth still has payload and padding.
	p := paddingConn{readPadding: 7}
	r := bytes.NewReader([]byte{0, 3, 2, 'a', 'b', 'c', 0, 0, 'x'})
	got := make([]byte, 1)
	for _, want := range []byte("abcx") {
		if p.readerReplaceable() {
			if _, err := r.Read(got); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := p.readWithPadding(r, got); err != nil {
				t.Fatal(err)
			}
		}
		if got[0] != want {
			t.Fatalf("padding leaked into payload: got %q want %q", got[0], want)
		}
	}
	if !p.readerReplaceable() {
		t.Fatal("completed padding still blocks unwrapping")
	}
}

func TestPaddingEmptyReadDoesNotConsumeHeader(t *testing.T) {
	p := paddingConn{}
	r := bytes.NewReader([]byte{0, 1, 0, 'x'})
	if n, err := p.readWithPadding(r, nil); n != 0 || err != nil {
		t.Fatalf("%d %v", n, err)
	}
	if r.Len() != 4 || p.readPadding != 0 {
		t.Fatal("empty read consumed frame header")
	}
}

type finalDataReader struct{ *bytes.Reader }

func (r finalDataReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.Len() == 0 {
		err = io.EOF
	}
	return n, err
}

func TestPaddingAccountsBytesReturnedWithEOF(t *testing.T) {
	p := paddingConn{readPadding: 8, readRemaining: 3}
	got := make([]byte, 3)
	n, err := p.readWithPadding(finalDataReader{bytes.NewReader([]byte("abc"))}, got)
	if n != 3 || err != io.EOF || p.readRemaining != 0 {
		t.Fatalf("n=%d err=%v remaining=%d", n, err, p.readRemaining)
	}
}
