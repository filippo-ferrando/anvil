package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"slices"

	"go.etcd.io/bbolt"

	"git.fferrando.cc/rdfilippo/anvil/internal/instance"
)

var bucketEvents = []byte("events") // big-endian seq -> json(instance.HistoryEvent)

// MaxEvents is how many events the log keeps; each append past it drops the oldest.
const MaxEvents = 10000

func seqKey(seq uint64) []byte {
	return binary.BigEndian.AppendUint64(nil, seq)
}

// AppendEvent stores ev under the bucket's next sequence number, dropping the event
// MaxEvents behind it, so the log never holds more than MaxEvents.
func (s *Store) AppendEvent(ev instance.HistoryEvent) (instance.HistoryEvent, error) {
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketEvents)
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		ev.Seq = seq
		data, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("store: encoding event: %w", err)
		}
		if err := b.Put(seqKey(seq), data); err != nil {
			return err
		}
		if seq > MaxEvents {
			return b.Delete(seqKey(seq - MaxEvents))
		}
		return nil
	})
	return ev, err
}

// ListEvents returns the newest limit events of instanceName (every instance when empty),
// oldest first. limit 0 returns every stored match.
func (s *Store) ListEvents(instanceName string, limit int) ([]instance.HistoryEvent, error) {
	// Events of other instances are skipped without decoding: a match must hold this field.
	var needle []byte
	if instanceName != "" {
		name, err := json.Marshal(instanceName)
		if err != nil {
			return nil, err
		}
		needle = append([]byte(`"InstanceName":`), name...)
	}
	var out []instance.HistoryEvent
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketEvents).Cursor()
		for k, data := c.Last(); k != nil; k, data = c.Prev() {
			if needle != nil && !bytes.Contains(data, needle) {
				continue
			}
			var ev instance.HistoryEvent
			if err := json.Unmarshal(data, &ev); err != nil {
				return err
			}
			if instanceName != "" && ev.InstanceName != instanceName {
				continue
			}
			out = append(out, ev)
			if limit > 0 && len(out) == limit {
				break
			}
		}
		return nil
	})
	slices.Reverse(out)
	return out, err
}
