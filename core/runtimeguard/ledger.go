package runtimeguard

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type record struct {
	Version  int
	Sequence uint64
	Previous string
	Kind     string
	Payload  json.RawMessage
	Hash     string
}

// Ledger is a private, single-writer issuance audit log, NOT a migration recovery
// checkpoint. Opening a previous directory is deliberately unsupported in v1.
type Ledger struct {
	mu       sync.Mutex
	file     *os.File
	sequence uint64
	head     string
	failed   bool
}

func NewLedger(dir string) (*Ledger, error) {
	if !filepath.IsAbs(dir) {
		return nil, ErrUntrusted
	}
	if e := os.Mkdir(dir, 0700); e != nil {
		return nil, e
	}
	f, e := os.OpenFile(filepath.Join(dir, "issuance.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	d, e := os.Open(dir)
	if e == nil {
		e = d.Sync()
		d.Close()
	}
	if e == nil {
		// Durably link the new independent session directory as well as its log.
		parent, openErr := os.Open(filepath.Dir(dir))
		e = openErr
		if e == nil {
			e = parent.Sync()
			parent.Close()
		}
	}
	if e != nil {
		f.Close()
		return nil, e
	}
	return &Ledger{file: f}, nil
}
func (l *Ledger) Append(kind string, payload any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failed {
		return ErrUntrusted
	}
	raw, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	r := record{Version: 1, Sequence: l.sequence + 1, Previous: l.head, Kind: kind, Payload: raw}
	r.Hash = hashValue(r)
	if e = writeFrame(l.file, r); e == nil {
		e = l.file.Sync()
	}
	if e != nil {
		l.failed = true
		return e
	}
	l.sequence = r.Sequence
	l.head = r.Hash
	return nil
}
func (l *Ledger) Close() error { return l.file.Close() }

// VerifyLedger is diagnostic only. Valid bytes do not authorize resuming old
// processes, copying a DB prefix, or reissuing any prior proof.
func VerifyLedger(in io.Reader) error {
	r := bufio.NewReaderSize(in, maxFrame+1)
	var seq uint64
	var head string
	for {
		var x record
		e := readFrame(r, &x)
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return ErrUntrusted
		}
		hash := x.Hash
		x.Hash = ""
		if x.Version != 1 || x.Sequence != seq+1 || x.Previous != head || hash != hashValue(x) {
			return ErrUntrusted
		}
		seq = x.Sequence
		head = hash
	}
}
