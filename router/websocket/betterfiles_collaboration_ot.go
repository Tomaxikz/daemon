package websocket

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"unicode/utf16"
	"unicode/utf8"

	ot "github.com/shiv248/operational-transformation-go"
)

const betterFilesCollabHistoryLimit = 128

var (
	errBetterFilesCollabBaseNotFound = errors.New("collaboration patch base is no longer available")
	errBetterFilesCollabInvalidEdit  = errors.New("invalid collaboration text edit")
	errBetterFilesCollabTooLarge     = errors.New("collaboration document is too large")
)

type betterFilesCollabDocument struct {
	sync.Mutex
	Content  string
	Revision int64
	History  []betterFilesCollabOperation
}

type betterFilesCollabOperation struct {
	Revision   int64
	BaseHash   string
	BaseLength int
	TargetHash string
	TargetLen  int
	Forward    *ot.OperationSeq
	Inverse    *ot.OperationSeq
}

type betterFilesCollabApplyResult struct {
	Changes     []betterFilesCollabTextEdit
	Revision    int64
	Transformed bool
}

func newBetterFilesCollabDocument(content string) *betterFilesCollabDocument {
	return &betterFilesCollabDocument{Content: content}
}

func (d *betterFilesCollabDocument) replace(content string) {
	d.Lock()
	d.Content = content
	d.Revision++
	d.History = nil
	d.Unlock()
}

func (d *betterFilesCollabDocument) snapshot() (string, int64) {
	d.Lock()
	defer d.Unlock()
	return d.Content, d.Revision
}

func (d *betterFilesCollabDocument) applyMonacoChanges(changes []betterFilesCollabTextEdit, maxBytes int) (betterFilesCollabApplyResult, error) {
	d.Lock()
	defer d.Unlock()

	baseRevision, baseContent, err := d.resolveBase(changes)
	if err != nil {
		return betterFilesCollabApplyResult{}, err
	}

	incoming, err := betterFilesCollabChangesToOperation(baseContent, changes)
	if err != nil {
		return betterFilesCollabApplyResult{}, err
	}

	for _, historical := range d.History {
		if historical.Revision <= baseRevision {
			continue
		}

		transformed, _, transformErr := incoming.Transform(historical.Forward)
		if transformErr != nil {
			return betterFilesCollabApplyResult{}, fmt.Errorf("transform collaboration patch: %w", transformErr)
		}
		incoming = transformed
	}

	current := d.Content
	normalized, err := betterFilesCollabOperationToChanges(current, incoming)
	if err != nil {
		return betterFilesCollabApplyResult{}, err
	}
	next, err := incoming.Apply(current)
	if err != nil {
		return betterFilesCollabApplyResult{}, fmt.Errorf("apply collaboration patch: %w", err)
	}
	if maxBytes > 0 && len(next) > maxBytes {
		return betterFilesCollabApplyResult{}, errBetterFilesCollabTooLarge
	}

	d.Revision++
	d.History = append(d.History, betterFilesCollabOperation{
		Revision:   d.Revision,
		BaseHash:   betterFilesCollabContentHash(current),
		BaseLength: betterFilesCollabUTF16Len(current),
		TargetHash: betterFilesCollabContentHash(next),
		TargetLen:  betterFilesCollabUTF16Len(next),
		Forward:    incoming,
		Inverse:    incoming.Invert(current),
	})
	if len(d.History) > betterFilesCollabHistoryLimit {
		d.History = append([]betterFilesCollabOperation(nil), d.History[len(d.History)-betterFilesCollabHistoryLimit:]...)
	}
	d.Content = next

	return betterFilesCollabApplyResult{
		Changes:     normalized,
		Revision:    d.Revision,
		Transformed: baseRevision != d.Revision-1,
	}, nil
}

func (d *betterFilesCollabDocument) resolveBase(changes []betterFilesCollabTextEdit) (int64, string, error) {
	if len(changes) == 0 {
		return 0, "", errBetterFilesCollabInvalidEdit
	}

	baseHash := changes[0].BaseHash
	baseLength := changes[0].BaseLength
	for _, change := range changes[1:] {
		if change.BaseHash != baseHash || change.BaseLength != baseLength {
			return 0, "", errBetterFilesCollabInvalidEdit
		}
	}

	// Older clients omit the base fingerprint for larger files. Those edits can
	// only be applied to the current revision because a stale base is ambiguous.
	if baseHash == "" {
		return d.Revision, d.Content, nil
	}
	if baseLength < 0 {
		return 0, "", errBetterFilesCollabInvalidEdit
	}
	if betterFilesCollabContentHash(d.Content) == baseHash && betterFilesCollabUTF16Len(d.Content) == baseLength {
		return d.Revision, d.Content, nil
	}

	baseRevision := int64(-1)
	for _, historical := range d.History {
		if historical.BaseHash == baseHash && historical.BaseLength == baseLength {
			baseRevision = historical.Revision - 1
			break
		}
		if historical.TargetHash == baseHash && historical.TargetLen == baseLength {
			baseRevision = historical.Revision
			break
		}
	}
	if baseRevision < 0 {
		return 0, "", errBetterFilesCollabBaseNotFound
	}

	baseContent := d.Content
	for i := len(d.History) - 1; i >= 0 && d.History[i].Revision > baseRevision; i-- {
		var err error
		baseContent, err = d.History[i].Inverse.Apply(baseContent)
		if err != nil {
			return 0, "", fmt.Errorf("reconstruct collaboration patch base: %w", err)
		}
	}
	if betterFilesCollabContentHash(baseContent) != baseHash || betterFilesCollabUTF16Len(baseContent) != baseLength {
		return 0, "", errBetterFilesCollabBaseNotFound
	}

	return baseRevision, baseContent, nil
}

func betterFilesCollabChangesToOperation(content string, changes []betterFilesCollabTextEdit) (*ot.OperationSeq, error) {
	if !utf8.ValidString(content) || len(changes) == 0 {
		return nil, errBetterFilesCollabInvalidEdit
	}

	sorted := append([]betterFilesCollabTextEdit(nil), changes...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].RangeOffset < sorted[j].RangeOffset
	})

	runes := []rune(content)
	prefix := betterFilesCollabUTF16Prefix(runes)
	op := ot.WithCapacity(len(sorted)*3 + 1)
	cursor := 0
	for _, change := range sorted {
		if change.RangeOffset < 0 || change.RangeLength < 0 || !utf8.ValidString(change.Text) {
			return nil, errBetterFilesCollabInvalidEdit
		}
		start, ok := betterFilesCollabRuneOffset(prefix, change.RangeOffset)
		if !ok {
			return nil, errBetterFilesCollabInvalidEdit
		}
		end, ok := betterFilesCollabRuneOffset(prefix, change.RangeOffset+change.RangeLength)
		if !ok || start < cursor || end < start {
			return nil, errBetterFilesCollabInvalidEdit
		}

		op.Retain(uint64(start - cursor))
		op.Delete(uint64(end - start))
		op.Insert(change.Text)
		cursor = end
	}
	op.Retain(uint64(len(runes) - cursor))
	return op, nil
}

func betterFilesCollabOperationToChanges(content string, operation *ot.OperationSeq) ([]betterFilesCollabTextEdit, error) {
	if operation == nil || operation.BaseLen() != utf8.RuneCountInString(content) {
		return nil, errBetterFilesCollabInvalidEdit
	}

	runes := []rune(content)
	prefix := betterFilesCollabUTF16Prefix(runes)
	baseHash := betterFilesCollabContentHash(content)
	baseLength := prefix[len(prefix)-1]
	changes := make([]betterFilesCollabTextEdit, 0, len(operation.Ops()))
	sourceRune := 0
	var pending *betterFilesCollabTextEdit
	flush := func() {
		if pending != nil && (pending.RangeLength != 0 || pending.Text != "") {
			changes = append(changes, *pending)
		}
		pending = nil
	}

	for _, raw := range operation.Ops() {
		switch value := raw.(type) {
		case ot.Retain:
			flush()
			sourceRune += int(value.N)
		case ot.Insert:
			if pending == nil {
				pending = &betterFilesCollabTextEdit{RangeOffset: prefix[sourceRune], BaseHash: baseHash, BaseLength: baseLength}
			}
			pending.Text += value.Text
		case ot.Delete:
			end := sourceRune + int(value.N)
			if end > len(runes) {
				return nil, errBetterFilesCollabInvalidEdit
			}
			if pending == nil {
				pending = &betterFilesCollabTextEdit{RangeOffset: prefix[sourceRune], BaseHash: baseHash, BaseLength: baseLength}
			}
			pending.RangeLength += prefix[end] - prefix[sourceRune]
			sourceRune = end
		default:
			return nil, errBetterFilesCollabInvalidEdit
		}
	}
	flush()
	if sourceRune != len(runes) {
		return nil, errBetterFilesCollabInvalidEdit
	}
	return changes, nil
}

func betterFilesCollabUTF16Prefix(runes []rune) []int {
	prefix := make([]int, len(runes)+1)
	for i, value := range runes {
		prefix[i+1] = prefix[i] + 1
		if value > 0xffff {
			prefix[i+1]++
		}
	}
	return prefix
}

func betterFilesCollabRuneOffset(prefix []int, utf16Offset int) (int, bool) {
	index := sort.SearchInts(prefix, utf16Offset)
	return index, index < len(prefix) && prefix[index] == utf16Offset
}

func betterFilesCollabUTF16Len(content string) int {
	return len(utf16.Encode([]rune(content)))
}

// betterFilesCollabContentHash matches the panel's JavaScript charCodeAt hash.
func betterFilesCollabContentHash(content string) string {
	var hash uint32
	for _, unit := range utf16.Encode([]rune(content)) {
		hash = hash*31 + uint32(unit)
	}
	return fmt.Sprintf("%08x", hash)
}
