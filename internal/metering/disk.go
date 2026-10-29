package metering

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type diskImage struct {
	SHA256 string
	Data   []byte
}

func encodeSnapshot(s snapshot) ([]byte, error) {
	data, e := json.Marshal(s)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(data)
	return json.Marshal(diskImage{SHA256: hex.EncodeToString(sum[:]), Data: data})
}
func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if e := d.Decode(out); e != nil {
		return e
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return ErrCorrupt
	}
	return nil
}

// Open requires an absolute private directory and trusted parent directories.
// It creates only that final directory. One owner holds a nonblocking OS file
// lock until Close. Linux and macOS are supported; other platforms fail closed.
func Open(dir string, o Options) (*WAL, error) {
	o, e := normalized(o)
	if e != nil {
		return nil, e
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) == string(filepath.Separator) {
		return nil, errors.New("absolute private WAL directory required")
	}
	created := false
	if e = os.Mkdir(dir, 0700); e != nil && !errors.Is(e, os.ErrExist) {
		return nil, e
	} else if e == nil {
		created = true
	}
	info, e := os.Lstat(dir)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("WAL directory must be private and not a symlink")
	}
	if created {
		if e = syncDirectory(filepath.Dir(dir)); e != nil {
			return nil, e
		}
	}
	_, lockErr := os.Lstat(filepath.Join(dir, "wal.lock"))
	oldLock := lockErr == nil
	if lockErr != nil && !errors.Is(lockErr, os.ErrNotExist) {
		return nil, lockErr
	}
	unlock, e := lockDirectory(dir)
	if e != nil {
		return nil, e
	}
	w := &WAL{dir: dir, unlock: unlock}
	ok := false
	defer func() {
		if !ok {
			_ = unlock()
		}
	}()
	w.persist = func(ctx context.Context, data []byte) error { return atomicSnapshot(ctx, dir, data) }
	data, e := readBounded(filepath.Join(dir, "wal.json"), o.MaxBytes)
	if errors.Is(e, os.ErrNotExist) {
		// Never silently recreate identity/epochs after losing a previously-owned WAL.
		if oldLock {
			return nil, ErrCorrupt
		}
		if _, e = os.Lstat(filepath.Join(dir, "wal.next")); e == nil || !errors.Is(e, os.ErrNotExist) {
			return nil, ErrCorrupt
		}
		id, e := randomID()
		if e != nil {
			return nil, e
		}
		state := snapshot{Version: walVersion, Options: o, ID: id}
		if e = w.commit(context.Background(), state); e != nil {
			return nil, e
		}
	} else {
		if e != nil {
			return nil, e
		}
		var image diskImage
		if e = strictJSON(data, &image); e != nil {
			return nil, errors.Join(ErrCorrupt, e)
		}
		sum := sha256.Sum256(image.Data)
		if image.SHA256 != hex.EncodeToString(sum[:]) {
			return nil, ErrCorrupt
		}
		var state snapshot
		if e = strictJSON(image.Data, &state); e != nil {
			return nil, errors.Join(ErrCorrupt, e)
		}
		if e = validateSnapshot(state, o); e != nil {
			return nil, e
		}
		w.state = state
	}
	ok = true
	return w, nil
}

func epochNumber(s snapshot, epoch string) (int64, bool) {
	prefix := "wal-" + s.ID + "-"
	if !strings.HasPrefix(epoch, prefix) {
		return 0, false
	}
	n, e := strconv.ParseInt(strings.TrimPrefix(epoch, prefix), 16, 64)
	return n, e == nil && n > 0 && n <= s.EpochNumber && epoch == fmt.Sprintf("%s%x", prefix, n)
}

func validateSnapshot(s snapshot, o Options) error {
	id, e := hex.DecodeString(s.ID)
	if e != nil || len(id) != 16 || s.ID != hex.EncodeToString(id) || s.Version != walVersion || s.Options != o || s.EpochNumber < 0 || len(s.Streams) > o.MaxStreams || len(s.Pending) > o.MaxPending || len(s.Totals) > o.MaxStreams || len(s.Incarnation) > 128 {
		return ErrCorrupt
	}
	if s.Incarnation != "" && !incarnationName.MatchString(s.Incarnation) {
		return ErrCorrupt
	}
	totals := map[string]periodTotal{}
	for _, p := range s.Totals {
		key := p.RuleID + "/" + p.SubscriptionID
		if _, exists := totals[key]; exists || !uuidPattern.MatchString(p.RuleID) || p.SubscriptionID != "" && !uuidPattern.MatchString(p.SubscriptionID) {
			return ErrCorrupt
		}
		if _, _, ok := effectiveUsage(p); !ok {
			return ErrCorrupt
		}
		totals[key] = p
	}
	if s.EpochNumber == 0 {
		if s.Epoch != "" || s.Incarnation != "" || len(s.Streams) != 0 || len(s.Pending) != 0 {
			return ErrCorrupt
		}
		for _, p := range s.Totals {
			if p.UploadBytes != 0 || p.DownloadBytes != 0 {
				return ErrCorrupt
			}
		}
		return nil
	}
	if n, ok := epochNumber(s, s.Epoch); !ok || n != s.EpochNumber {
		return ErrCorrupt
	}
	cursors := map[string]stream{}
	retained := map[string]periodTotal{}
	activeRules := map[string]bool{}
	lastRule := ""
	for _, c := range s.Streams {
		key := c.RuleID + "/" + c.Epoch
		if _, ok := cursors[key]; ok {
			return ErrCorrupt
		}
		cursors[key] = c
		groups, e := groupReadings(c.Readings)
		if e != nil || len(groups) != 1 || !reflect.DeepEqual(groups[0], c.group) || c.Sequence < 1 {
			return ErrCorrupt
		}
		if _, ok := epochNumber(s, c.Epoch); !ok {
			return ErrCorrupt
		}
		periodKey := c.RuleID + "/" + c.SubscriptionID
		p, exists := totals[periodKey]
		r := retained[periodKey]
		if !exists || c.UploadBytes > p.UploadBytes-r.UploadBytes || c.DownloadBytes > p.DownloadBytes-r.DownloadBytes {
			return ErrCorrupt
		}
		r.UploadBytes += c.UploadBytes
		r.DownloadBytes += c.DownloadBytes
		retained[periodKey] = r
		if c.Active {
			if c.Epoch != s.Epoch || activeRules[c.RuleID] || c.RuleID <= lastRule {
				return ErrCorrupt
			}
			activeRules[c.RuleID] = true
			lastRule = c.RuleID
		}
	}
	last := map[string]Report{}
	for _, payload := range s.Pending {
		var r Report
		if e := strictJSON(payload, &r); e != nil {
			return ErrCorrupt
		}
		canonical, e := json.Marshal(r)
		if e != nil || !bytes.Equal(canonical, payload) {
			return ErrCorrupt
		}
		key := r.RuleID + "/" + r.Epoch
		c, ok := cursors[key]
		if !ok || r.SubscriptionID != c.SubscriptionID || r.Sequence < 1 || r.Sequence > c.Sequence || !bytePair(r.UploadBytes, r.DownloadBytes) || r.UploadBytes > c.UploadBytes || r.DownloadBytes > c.DownloadBytes {
			return ErrCorrupt
		}
		if old, ok := last[key]; ok && (r.Sequence != old.Sequence+1 || r.UploadBytes < old.UploadBytes || r.DownloadBytes < old.DownloadBytes) {
			return ErrCorrupt
		}
		last[key] = r
	}
	for key, c := range cursors {
		if r, ok := last[key]; ok {
			if r.Sequence != c.Sequence || r.UploadBytes != c.UploadBytes || r.DownloadBytes != c.DownloadBytes {
				return ErrCorrupt
			}
		} else if !c.Active {
			return ErrCorrupt
		}
	}
	return nil
}

func privateRegular(f *os.File) error {
	info, e := f.Stat()
	if e != nil {
		return e
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !singleLink(info) {
		return errors.New("unsafe WAL file")
	}
	return nil
}
func readBounded(path string, max int64) ([]byte, error) {
	f, e := openPrivate(path, os.O_RDONLY)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	if e = privateRegular(f); e != nil {
		return nil, e
	}
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if info.Size() > max {
		return nil, ErrCorrupt
	}
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > max {
		return nil, ErrCorrupt
	}
	return b, nil
}
func syncDirectory(path string) error {
	d, e := os.Open(path)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

// Only two bounded image slots exist: wal.json (committed) and wal.next (scratch).
// Startup ignores scratch when a committed image exists. No timestamp can make
// scratch authoritative. Failure poisons the owning WAL until it is reopened.
func atomicSnapshot(ctx context.Context, dir string, data []byte) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	scratch := filepath.Join(dir, "wal.next")
	f, e := openPrivate(scratch, os.O_CREATE|os.O_RDWR)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = privateRegular(f); e != nil {
		return e
	}
	if e = f.Truncate(0); e != nil {
		return e
	}
	if _, e = io.Copy(f, bytes.NewReader(data)); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = os.Rename(scratch, filepath.Join(dir, "wal.json")); e != nil {
		return e
	}
	if e = syncDirectory(dir); e != nil {
		return errors.Join(ErrUncertain, e)
	}
	return nil
}
