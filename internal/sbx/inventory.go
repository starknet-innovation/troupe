package sbx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Record is one sandbox in sbx ls --json, which prints a bare array of them (sbx 0.48).
type Record struct {
	Name      string `json:"name"`
	ID        string `json:"id"`
	Status    string `json:"status"`
	CPUs      int    `json:"cpus"`
	MemoryMiB int    `json:"memory_mib"`
}

// ErrInventory means sbx ls --json was not a readable inventory.
var ErrInventory = errors.New("sandbox inventory unreadable")

// Inventory lists every sandbox by name; a name held by two sandboxes is an error.
func (r Runner) Inventory(ctx context.Context) (map[string]Record, error) {
	out, err := r.Output(ctx, Call{Args: []string{"ls", "--json"}, Timeout: 30 * time.Second})
	if err != nil {
		return nil, err
	}
	// Anything but an array, null included, is unreadable: the older {"sandboxes": [...]}
	// shape too, so a mismatched sbx never reads as an empty machine.
	var inventory []Record
	if err := json.Unmarshal(out, &inventory); err != nil || inventory == nil {
		return nil, ErrInventory
	}
	records := map[string]Record{}
	for _, record := range inventory {
		if _, taken := records[record.Name]; taken {
			return nil, fmt.Errorf("%w: two sandboxes are named %s", ErrInventory, record.Name)
		}
		records[record.Name] = record
	}
	return records, nil
}
