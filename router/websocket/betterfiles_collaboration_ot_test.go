package websocket

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func betterFilesTestEdit(base string, offset int, length int, text string) betterFilesCollabTextEdit {
	return betterFilesCollabTextEdit{
		RangeOffset: offset,
		RangeLength: length,
		Text:        text,
		BaseHash:    betterFilesCollabContentHash(base),
		BaseLength:  betterFilesCollabUTF16Len(base),
	}
}

func betterFilesTestContent(document *betterFilesCollabDocument) string {
	content, _ := document.snapshot()
	return content
}

func TestBetterFilesCollabAppliesMonacoReplacement(t *testing.T) {
	document := newBetterFilesCollabDocument("first line\nold line\nlast line")
	result, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{
		betterFilesTestEdit(document.Content, 11, 8, "new line"),
	}, 0)

	require.NoError(t, err)
	require.Equal(t, "first line\nnew line\nlast line", betterFilesTestContent(document))
	require.False(t, result.Transformed)
	require.Equal(t, int64(1), result.Revision)
	require.Equal(t, []betterFilesCollabTextEdit{{
		RangeOffset: 11,
		RangeLength: 8,
		Text:        "new line",
		BaseHash:    betterFilesCollabContentHash("first line\nold line\nlast line"),
		BaseLength:  betterFilesCollabUTF16Len("first line\nold line\nlast line"),
	}}, result.Changes)
}

func TestBetterFilesCollabTransformsConcurrentInserts(t *testing.T) {
	const base = "ab"
	document := newBetterFilesCollabDocument(base)

	_, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(base, 1, 0, "X")}, 0)
	require.NoError(t, err)
	require.Equal(t, "aXb", betterFilesTestContent(document))

	second, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(base, 1, 0, "Y")}, 0)
	require.NoError(t, err)
	require.True(t, second.Transformed)
	require.Equal(t, "aXYb", betterFilesTestContent(document))
	require.Equal(t, 2, second.Changes[0].RangeOffset)
	require.Equal(t, betterFilesCollabContentHash("aXb"), second.Changes[0].BaseHash)
}

func TestBetterFilesCollabTransformsStaleEditAcrossSeveralRevisions(t *testing.T) {
	const base = "hello world"
	document := newBetterFilesCollabDocument(base)

	_, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(base, 0, 0, "A ")}, 0)
	require.NoError(t, err)
	current := document.Content
	_, err = document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(current, betterFilesCollabUTF16Len(current), 0, "!")}, 0)
	require.NoError(t, err)

	result, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(base, 6, 5, "Wings")}, 0)
	require.NoError(t, err)
	require.True(t, result.Transformed)
	require.Equal(t, "A hello Wings!", betterFilesTestContent(document))
}

func TestBetterFilesCollabConvertsMonacoUTF16Offsets(t *testing.T) {
	const base = "a😀b"
	document := newBetterFilesCollabDocument(base)

	_, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(base, 3, 1, "c")}, 0)
	require.NoError(t, err)
	require.Equal(t, "a😀c", betterFilesTestContent(document))

	document = newBetterFilesCollabDocument(base)
	_, err = document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(base, 2, 0, "x")}, 0)
	require.ErrorIs(t, err, errBetterFilesCollabInvalidEdit)
}

func TestBetterFilesCollabRejectsOverlappingChanges(t *testing.T) {
	const base = "abcdef"
	document := newBetterFilesCollabDocument(base)

	_, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{
		betterFilesTestEdit(base, 1, 3, "x"),
		betterFilesTestEdit(base, 2, 1, "y"),
	}, 0)
	require.ErrorIs(t, err, errBetterFilesCollabInvalidEdit)
}

func TestBetterFilesCollabRejectsExpiredPatchBase(t *testing.T) {
	const base = "x"
	document := newBetterFilesCollabDocument(base)

	for i := 0; i < betterFilesCollabHistoryLimit+1; i++ {
		current := document.Content
		_, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{
			betterFilesTestEdit(current, betterFilesCollabUTF16Len(current), 0, fmt.Sprint(i%10)),
		}, 0)
		require.NoError(t, err)
	}

	_, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit(base, 0, 1, "y")}, 0)
	require.True(t, errors.Is(err, errBetterFilesCollabBaseNotFound), "unexpected error: %v", err)
}

func TestBetterFilesCollabReplaceResetsOperationHistory(t *testing.T) {
	document := newBetterFilesCollabDocument("before")
	_, err := document.applyMonacoChanges([]betterFilesCollabTextEdit{betterFilesTestEdit("before", 0, 0, "x")}, 0)
	require.NoError(t, err)
	require.NotEmpty(t, document.History)

	document.replace("saved")
	content, revision := document.snapshot()
	require.Equal(t, "saved", content)
	require.Equal(t, int64(2), revision)
	require.Empty(t, document.History)
}
