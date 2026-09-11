package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/internal/ufs"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

func searchV2Number(value int64) *int64 { return &value }

func searchV2Names(results []betterFilesEntry) []string {
	names := make([]string, 0, len(results))
	for _, result := range results {
		names = append(names, result.Name)
	}
	return names
}

func runSearchV2Test(t *testing.T, fs *serverfs.Filesystem, root string, data searchV2Payload) []betterFilesEntry {
	t.Helper()
	if data.PerPage == 0 {
		data.PerPage = 100
	}
	require.NoError(t, validateSearchV2Payload(&data))
	results, err := runSearchV2(context.Background(), fs, root, data, nil)
	require.NoError(t, err)
	return results
}

func requestSearchV2(handler http.Handler, serverID, body string, ctx context.Context, authorized bool) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/api/servers/"+serverID+"/files/search", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	if authorized {
		request.Header.Set("Authorization", "Bearer "+config.Get().Token.Token)
	}
	if ctx != nil {
		request = request.WithContext(ctx)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSearchV2HTTPBFMContractAndRelativePaths(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	fs := s.Filesystem()
	writeBetterFilesFixture(t, fs, "/plugins/a/config.yml", []byte("enabled: true"))
	writeBetterFilesFixture(t, fs, "/plugins/b/config.yml", []byte("ENABLED: false"))
	writeBetterFilesFixture(t, fs, "/plugins/b/cache/config.yml", []byte("enabled"))
	writeBetterFilesFixture(t, fs, "/outside/config.yml", []byte("enabled"))
	body := `{"root":"/plugins","path_filter":{"include":["**/*.yml"],"exclude":["**/cache/**"],"case_insensitive":true},"size_filter":{"min":0,"max":10485760},"content_filter":{"query":"enabled","max_search_size":2097152,"include_unmatched":false,"case_insensitive":true},"per_page":100}`
	response := requestSearchV2(handler, s.ID(), body, nil, true)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var payload struct {
		Results []betterFilesEntry `json:"results"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.ElementsMatch(t, []string{"a/config.yml", "b/config.yml"}, searchV2Names(payload.Results))
	for _, entry := range payload.Results {
		require.True(t, entry.File)
		require.False(t, entry.Directory)
		require.False(t, entry.Symlink)
		require.NotEmpty(t, entry.Mime)
		require.NotEmpty(t, entry.Modified)
		require.False(t, strings.HasPrefix(entry.Name, "/"))
		require.NotContains(t, response.Body.String(), fs.Path())
	}
	// Leading-slash and slash-containing patterns are server-root anchored,
	// independently of the relative names returned for this non-root search.
	results := runSearchV2Test(t, fs, "/plugins", searchV2Payload{PathFilter: &searchV2PathFilter{Include: []string{"a/*.yml"}}})
	require.Empty(t, results)
	results = runSearchV2Test(t, fs, "/plugins", searchV2Payload{PathFilter: &searchV2PathFilter{Include: []string{"/plugins/a/*.yml"}}})
	require.Equal(t, []string{"a/config.yml"}, searchV2Names(results))
}

func TestSearchV2OmittedNullFiltersAndEmptyArrays(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	for _, body := range []string{`{}`, `{"root":null,"path_filter":null,"size_filter":null,"content_filter":null,"per_page":null}`, `{"path_filter":{"include":null,"exclude":null}}`} {
		response := requestSearchV2(handler, s.ID(), body, nil, true)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.JSONEq(t, `{"results":[]}`, response.Body.String())
	}
	writeBetterFilesFixture(t, s.Filesystem(), "/nested/file.txt", []byte("test"))
	response := requestSearchV2(handler, s.ID(), `{}`, nil, true)
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), "nested/file.txt")
	require.NotContains(t, response.Body.String(), `"name":"nested"`)
	response = requestSearchV2(handler, s.ID(), `{"per_page":0}`, nil, true)
	require.JSONEq(t, `{"results":[]}`, response.Body.String())
}

func TestSearchV2GlobContentUnicodeAndIgnoredFiles(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0, "logs/**", "ignored/**")
	writeBetterFilesFixture(t, fs, "/config/paper.yml", []byte("name: HéLLo 😀\n"))
	writeBetterFilesFixture(t, fs, "/config/other.yml", []byte("name: other\n"))
	writeBetterFilesFixture(t, fs, "/logs/server.yml", []byte("name: HéLLo 😀\n"))
	writeBetterFilesFixture(t, fs, "/ignored/secret.yml", []byte("name: HéLLo 😀\n"))
	writeBetterFilesFixture(t, fs, "/config/binary.yml", []byte{0xff, 'x'})
	results := runSearchV2Test(t, fs, "/", searchV2Payload{
		PathFilter:    &searchV2PathFilter{Include: []string{"**/*.yml"}, Exclude: []string{"**/other.yml"}, CaseInsensitive: true},
		ContentFilter: &searchV2ContentFilter{Query: "héllo 😀", MaxSearchSize: searchV2Number(1024), CaseInsensitive: true},
	})
	require.Equal(t, []string{"config/paper.yml"}, searchV2Names(results))
}

func TestSearchV2GlobOrderingExclusionsAndByteSemantics(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	for _, name := range []string{"/root.yml", "/nested/paper.yml", "/nested/PAPER.YML", "/nested/cache/keep.yml", "/nested/other.yml", "/é.yml", "/😀.yml", "/#literal.yml"} {
		writeBetterFilesFixture(t, fs, name, []byte("x"))
	}
	tests := []struct {
		name     string
		filter   searchV2PathFilter
		expected []string
	}{
		{"basename at any depth", searchV2PathFilter{Include: []string{"*.yml"}, Exclude: []string{"cache/"}, CaseInsensitive: true}, []string{"root.yml", "nested/paper.yml", "nested/PAPER.YML", "nested/other.yml", "é.yml", "😀.yml", "#literal.yml"}},
		{"server root anchor", searchV2PathFilter{Include: []string{"/*.yml"}}, []string{"root.yml", "é.yml", "😀.yml", "#literal.yml"}},
		{"ordered include negation", searchV2PathFilter{Include: []string{"*.yml", "!**/other.yml"}, Exclude: []string{"cache/"}}, []string{"root.yml", "nested/paper.yml", "é.yml", "😀.yml", "#literal.yml"}},
		{"ordered exclude reinclude", searchV2PathFilter{Include: []string{"*.yml"}, Exclude: []string{"*.yml", "!/root.yml"}}, []string{"root.yml"}},
		{"excluded parents stay pruned", searchV2PathFilter{Include: []string{"**"}, Exclude: []string{"cache/", "!nested/cache/keep.yml"}}, []string{"root.yml", "nested/paper.yml", "nested/PAPER.YML", "nested/other.yml", "é.yml", "😀.yml", "#literal.yml"}},
		{"brace and class", searchV2PathFilter{Include: []string{"**/[pr]*.{yml,yaml}"}, Exclude: []string{"cache/"}}, []string{"root.yml", "nested/paper.yml"}},
		{"UTF8 byte question marks", searchV2PathFilter{Include: []string{"/??.yml"}}, []string{"é.yml"}},
		{"emoji four bytes", searchV2PathFilter{Include: []string{"/????.yml"}}, []string{"😀.yml", "root.yml"}},
		{"escaped leading hash", searchV2PathFilter{Include: []string{"\\#literal.yml"}}, []string{"#literal.yml"}},
		{"case-sensitive default", searchV2PathFilter{Include: []string{"**/PAPER.YML"}}, []string{"nested/PAPER.YML"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			results := runSearchV2Test(t, fs, "/", searchV2Payload{PathFilter: &test.filter})
			require.ElementsMatch(t, test.expected, searchV2Names(results))
		})
	}
	// A trailing /** matches contents, not the directory node itself.
	filter := &searchV2PathFilter{Exclude: []string{"**/cache/**"}}
	require.NoError(t, validateSearchV2Payload(&searchV2Payload{PathFilter: filter}))
	_, excluded := searchV2PathDecision("/nested/cache", true, filter)
	require.False(t, excluded)
	_, excluded = searchV2PathDecision("/nested/cache/child", true, filter)
	require.True(t, excluded)
}

func TestSearchV2ASCIIOnlyCaseFolding(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/Ä.TXT", []byte("MiXeD Ä 😀"))
	for _, query := range []struct {
		query             string
		insensitive, want bool
	}{
		{"mixed", false, false}, {"mixed", true, true}, {"ä", true, false}, {"Ä 😀", true, true},
	} {
		results := runSearchV2Test(t, fs, "/", searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: query.query, CaseInsensitive: query.insensitive}})
		require.Equal(t, query.want, len(results) == 1)
	}
	for _, pattern := range []struct {
		pattern string
		want    bool
	}{{"ä.txt", false}, {"Ä.txt", true}} {
		results := runSearchV2Test(t, fs, "/", searchV2Payload{PathFilter: &searchV2PathFilter{Include: []string{pattern.pattern}, CaseInsensitive: true}})
		require.Equal(t, pattern.want, len(results) == 1)
	}
}

func TestSearchV2CharacterClassesPreserveRangesAndLiteralBrackets(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	for _, name := range []string{"_.txt", "].txt", "A.txt", "a.txt", "Z.txt", "z.txt", "b.txt"} {
		writeBetterFilesFixture(t, fs, "/"+name, nil)
	}
	for _, test := range []struct {
		pattern     string
		insensitive bool
		expected    []string
	}{
		{"[A-z].txt", true, []string{"_.txt", "].txt", "A.txt", "a.txt", "Z.txt", "z.txt", "b.txt"}},
		{"[Z-a].txt", true, []string{"_.txt", "].txt", "A.txt", "a.txt", "Z.txt", "z.txt"}},
		{"[]a].txt", false, []string{"].txt", "a.txt"}},
		{"[]a].txt", true, []string{"].txt", "a.txt", "A.txt"}},
		{"[!A-Z].txt", true, []string{"_.txt", "].txt"}},
	} {
		results := runSearchV2Test(t, fs, "/", searchV2Payload{PathFilter: &searchV2PathFilter{Include: []string{test.pattern}, CaseInsensitive: test.insensitive}})
		require.ElementsMatch(t, test.expected, searchV2Names(results), test.pattern)
	}
}

func TestSearchV2UnreadableFileDoesNotAbortReadableMatches(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 files")
	}
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	writeBetterFilesFixture(t, s.Filesystem(), "/good.txt", []byte("match"))
	writeBetterFilesFixture(t, s.Filesystem(), "/blocked.txt", []byte("match"))
	blocked := filepath.Join(s.Filesystem().Path(), "blocked.txt")
	require.NoError(t, os.Chmod(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o600) })
	response := requestSearchV2(handler, s.ID(), `{"content_filter":{"query":"match"}}`, nil, true)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var payload struct {
		Results []betterFilesEntry `json:"results"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &payload))
	require.Equal(t, []string{"good.txt"}, searchV2Names(payload.Results))
}

func TestSearchV2SizeBoundaries(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	for _, size := range []int{0, 4, 5, 6} {
		writeBetterFilesFixture(t, fs, fmt.Sprintf("/size%d", size), bytes.Repeat([]byte("x"), size))
	}
	results := runSearchV2Test(t, fs, "/", searchV2Payload{SizeFilter: &searchV2SizeFilter{Min: searchV2Number(4), Max: searchV2Number(6)}})
	require.ElementsMatch(t, []string{"size4", "size5"}, searchV2Names(results))
	results = runSearchV2Test(t, fs, "/", searchV2Payload{SizeFilter: &searchV2SizeFilter{Min: searchV2Number(5), Max: searchV2Number(5)}})
	require.Empty(t, results)
	results = runSearchV2Test(t, fs, "/", searchV2Payload{SizeFilter: &searchV2SizeFilter{Max: searchV2Number(0)}})
	require.Empty(t, results)
	require.NoError(t, validateSearchV2Payload(&searchV2Payload{SizeFilter: &searchV2SizeFilter{}}))
}

func TestSearchV2IncludeUnmatchedBinaryAndEmptyQueries(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/match.txt", []byte("match"))
	writeBetterFilesFixture(t, fs, "/other.txt", []byte("other"))
	writeBetterFilesFixture(t, fs, "/large.txt", []byte("a large file without the needle"))
	writeBetterFilesFixture(t, fs, "/binary-small", []byte{0xff, 'm', 'a', 't', 'c', 'h'})
	writeBetterFilesFixture(t, fs, "/binary-large", append([]byte{0xff}, bytes.Repeat([]byte("x"), 30)...))
	writeBetterFilesFixture(t, fs, "/nul.bin", []byte{'m', 'a', 0, 't', 'c', 'h'})
	writeBetterFilesFixture(t, fs, "/empty.txt", nil)
	results := runSearchV2Test(t, fs, "/", searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: "match", MaxSearchSize: searchV2Number(6), IncludeUnmatched: true}})
	require.ElementsMatch(t, []string{"match.txt", "large.txt", "binary-large"}, searchV2Names(results))
	results = runSearchV2Test(t, fs, "/", searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: "", MaxSearchSize: searchV2Number(6)}})
	require.ElementsMatch(t, []string{"match.txt", "other.txt", "nul.bin", "empty.txt"}, searchV2Names(results))
	results = runSearchV2Test(t, fs, "/", searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: "", MaxSearchSize: searchV2Number(0)}})
	require.Equal(t, []string{"empty.txt"}, searchV2Names(results))
	results = runSearchV2Test(t, fs, "/", searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: "absent", MaxSearchSize: searchV2Number(0), IncludeUnmatched: true}})
	require.ElementsMatch(t, []string{"match.txt", "other.txt", "large.txt", "binary-small", "binary-large", "nul.bin"}, searchV2Names(results))
	// The upstream heuristic examines the head only; later non-UTF8 bytes and
	// MIME labels do not turn a valid head into a rejected content search.
	writeBetterFilesFixture(t, fs, "/late-binary", append(bytes.Repeat([]byte("a"), 128), 0xff, 'm', 'a', 't', 'c', 'h'))
	results = runSearchV2Test(t, fs, "/", searchV2Payload{PathFilter: &searchV2PathFilter{Include: []string{"late-binary"}}, ContentFilter: &searchV2ContentFilter{Query: "match"}})
	require.Len(t, results, 1)
	for _, input := range []struct {
		head  []byte
		valid bool
	}{
		{[]byte{0}, true}, {[]byte{0xff}, false}, {[]byte{0xe2, 0x82}, true}, {[]byte{0xe2, 'x'}, false}, {[]byte("�"), true},
	} {
		require.Equal(t, input.valid, validSearchV2TextHead(input.head))
	}
}

type searchV2CountingReader struct {
	io.Reader
	bytes      int
	maxRequest int
	cancel     context.CancelFunc
}

func (r *searchV2CountingReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRequest {
		r.maxRequest = len(p)
	}
	n, err := r.Reader.Read(p)
	r.bytes += n
	if r.cancel != nil {
		r.cancel()
	}
	return n, err
}

func TestStreamSearchV2TextAcrossBufferBoundariesAndReadLimits(t *testing.T) {
	content := append(bytes.Repeat([]byte("a"), 64*1024-2), []byte("a😀Target")...)
	reader := &searchV2CountingReader{Reader: bytes.NewReader(content)}
	matched, err := streamSearchV2Text(context.Background(), reader, int64(len(content)), newSearchV2Matcher("a😀target", true))
	require.NoError(t, err)
	require.True(t, matched)
	require.LessOrEqual(t, reader.maxRequest, 64*1024)
	reader = &searchV2CountingReader{Reader: strings.NewReader("a needle beyond the limit")}
	matched, err = streamSearchV2Text(context.Background(), reader, 3, newSearchV2Matcher("needle", false))
	require.NoError(t, err)
	require.False(t, matched)
	require.Equal(t, 3, reader.bytes)
	matched, err = streamSearchV2Text(context.Background(), strings.NewReader("match"), 5, newSearchV2Matcher("match", false))
	require.NoError(t, err)
	require.True(t, matched)
	ctx, cancel := context.WithCancel(context.Background())
	reader = &searchV2CountingReader{Reader: strings.NewReader("match"), cancel: cancel}
	_, err = streamSearchV2Text(ctx, reader, 5, newSearchV2Matcher("match", false))
	require.ErrorIs(t, err, context.Canceled)
}

func TestSearchV2HeadAndTotalReadBudget(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/large", bytes.Repeat([]byte("x"), 1024*1024))
	data := searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: "absent", MaxSearchSize: searchV2Number(0), IncludeUnmatched: true}, PerPage: 100}
	require.NoError(t, validateSearchV2Payload(&data))
	budget := &searchV2Budget{readRemaining: searchV2HeadBytes}
	results, err := runSearchV2(context.Background(), fs, "/", data, budget)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, int64(0), budget.readRemaining)
	data.ContentFilter.MaxSearchSize = searchV2Number(1024 * 1024)
	budget = &searchV2Budget{readRemaining: 256}
	_, err = runSearchV2(context.Background(), fs, "/", data, budget)
	require.ErrorIs(t, err, errSearchV2ReadLimit)
	data.ContentFilter.MaxSearchSize = searchV2Number(1024 * 1024 * 1024)
	require.NoError(t, validateSearchV2Payload(&data), "BFM allows up to 1 GiB")
	require.Equal(t, defaultSearchV2ReadLimit, (searchV2ContentFilter{}).readLimit())
}

func TestSearchV2HTTPInvalidInputsAndAuthorization(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10, "private/**")
	cases := []struct {
		body   string
		status int
	}{
		{`null`, 400}, {`[]`, 400}, {`{} {}`, 400}, {`{"include":["*.txt"]}`, 400},
		{`{"path_filter":{"unknown":true}}`, 400}, {`{"content_filter":{"max_search_size":"5"}}`, 400},
		{`{"match_context":{"before":1}}`, 400}, {`{"per_page":-1}`, 422},
		{`{"size_filter":{"min":-1}}`, 422}, {`{"size_filter":{"min":5,"max":4}}`, 422},
		{`{"content_filter":{"max_search_size":-1}}`, 422}, {`{"content_filter":{"max_search_size":1073741825}}`, 422},
		{`{"path_filter":{"include":["["]}}`, 422}, {`{"path_filter":{"include":["../*"]}}`, 422},
		{`{"path_filter":{"include":["{a,{b,c}}"]}}`, 422}, {`{"path_filter":{"include":["{a,}"]}}`, 422},
		{`{"root":"/../plugins"}`, 422}, {`{"root":"/private/deep"}`, 403}, {`{"root":"/missing"}`, 404},
	}
	for _, test := range cases {
		t.Run(test.body, func(t *testing.T) {
			response := requestSearchV2(handler, s.ID(), test.body, nil, true)
			require.Equal(t, test.status, response.Code, response.Body.String())
			require.Contains(t, response.Body.String(), `"error"`)
		})
	}
	response := requestSearchV2(handler, s.ID(), `{}`, nil, false)
	require.Equal(t, 401, response.Code)
	response = requestSearchV2(handler, s.ID(), strings.Repeat(" ", maxSearchV2RequestBytes)+`{}`, nil, true)
	require.Equal(t, 413, response.Code)
	response = requestSearchV2(handler, s.ID(), "{\"root\":\"/\xff\"}", nil, true)
	require.Equal(t, 400, response.Code)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	response = requestSearchV2(handler, s.ID(), `{}`, ctx, true)
	require.Equal(t, 408, response.Code)
}

func TestSearchV2PatternAndQueryLimits(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		valid    bool
	}{
		{"maximum count", strings.Split(strings.Repeat("a,", maxSearchV2Patterns-1)+"a", ","), true},
		{"too many", strings.Split(strings.Repeat("a,", maxSearchV2Patterns)+"a", ","), false},
		{"maximum bytes", strings.Split(strings.Repeat(strings.Repeat("a", 4096)+",", 15)+strings.Repeat("a", 4096), ","), true},
		{"too many bytes", strings.Split(strings.Repeat(strings.Repeat("a", 4096)+",", 16)+"a", ","), false},
		{"pattern too long", []string{strings.Repeat("a", 4097)}, false},
		{"64 combinations", []string{strings.Repeat("{a,b}", 6)}, true},
		{"128 combinations", []string{strings.Repeat("{a,b}", 7)}, false},
		{"cross-directory alternative", []string{"{plugins/a,config/b}.yml"}, false},
		{"empty pattern", []string{""}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			data := searchV2Payload{PathFilter: &searchV2PathFilter{Include: test.patterns}}
			err := validateSearchV2Payload(&data)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	data := searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: strings.Repeat("é", 2048)}}
	require.NoError(t, validateSearchV2Payload(&data))
	data.ContentFilter.Query += "a"
	require.Error(t, validateSearchV2Payload(&data))
}

func TestSearchV2RejectsSymlinkRootsAndTraversal(t *testing.T) {
	for _, openat2 := range []bool{false, true} {
		t.Run(fmt.Sprint(openat2), func(t *testing.T) {
			_, s, handler := newBetterFilesHTTPServer(t, 10, 10, "protected/**")
			fs := s.Filesystem()
			native, err := ufs.NewUnixFS(fs.Path(), openat2)
			require.NoError(t, err)
			*fs.UnixFS() = *native
			writeBetterFilesFixture(t, fs, "/protected/nested/deep/secret.txt", []byte("secret"))
			writeBetterFilesFixture(t, fs, "/safe/file.txt", []byte("safe"))
			require.NoError(t, os.Symlink("protected", filepath.Join(fs.Path(), "alias")))
			response := requestSearchV2(handler, s.ID(), `{"root":"/alias/nested/deep"}`, nil, true)
			require.Equal(t, 422, response.Code, response.Body.String())
			outside := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("secret"), 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(fs.Path(), "escape")))
			require.NoError(t, os.Symlink("../protected/nested/deep/secret.txt", filepath.Join(fs.Path(), "safe", "link")))
			results := runSearchV2Test(t, fs, "/", searchV2Payload{})
			require.Equal(t, []string{"safe/file.txt"}, searchV2Names(results))
		})
	}
}

func TestSearchV2PinnedDirectoryAndNonblockingFileOpen(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0, "protected/**")
	writeBetterFilesFixture(t, fs, "/safe/file.txt", []byte("safe"))
	writeBetterFilesFixture(t, fs, "/protected/file.txt", []byte("secret"))
	parent, err := openSearchV2Root(context.Background(), fs, "/safe")
	require.NoError(t, err)
	defer parent.Close()
	require.NoError(t, os.Rename(filepath.Join(fs.Path(), "safe"), filepath.Join(fs.Path(), "original")))
	require.NoError(t, os.Symlink("protected", filepath.Join(fs.Path(), "safe")))
	entry, matched, err := searchV2File(context.Background(), fs, int(parent.Fd()), "file.txt", "file.txt", searchV2Payload{ContentFilter: &searchV2ContentFilter{Query: "safe"}}, newSearchV2Matcher("safe", false), &searchV2Budget{readRemaining: 1024})
	require.NoError(t, err)
	require.True(t, matched)
	require.Equal(t, "file.txt", entry.Name)
	fifo := filepath.Join(fs.Path(), "original", "fifo")
	require.NoError(t, unix.Mkfifo(fifo, 0o600))
	done := make(chan error, 1)
	go func() {
		_, matched, err := searchV2File(context.Background(), fs, int(parent.Fd()), "fifo", "fifo", searchV2Payload{}, nil, &searchV2Budget{readRemaining: 1024})
		if err == nil && matched {
			err = errors.New("FIFO incorrectly included")
		}
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(time.Second):
		fd, err := unix.Open(fifo, unix.O_RDWR|unix.O_NONBLOCK, 0)
		if err == nil {
			defer unix.Close(fd)
		}
		t.Fatal("search open blocked on a FIFO")
	}
}

func TestSearchV2ResultTraversalDepthAndCancellationLimits(t *testing.T) {
	fs := newBetterFilesTestFilesystem(t, 0)
	for i := 0; i < maxSearchV2Results+5; i++ {
		writeBetterFilesFixture(t, fs, fmt.Sprintf("/file-%03d", i), nil)
	}
	data := searchV2Payload{PerPage: 5000}
	require.NoError(t, validateSearchV2Payload(&data))
	require.Equal(t, 500, data.PerPage)
	results, err := runSearchV2(context.Background(), fs, "/", data, nil)
	require.NoError(t, err)
	require.Len(t, results, 500)
	budget := &searchV2Budget{entriesVisited: maxSearchV2Entries - 1, readRemaining: 1024}
	_, err = runSearchV2(context.Background(), fs, "/", searchV2Payload{PerPage: 100}, budget)
	require.ErrorIs(t, err, errSearchV2TraversalLimit)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = runSearchV2(ctx, fs, "/", data, nil)
	require.ErrorIs(t, err, context.Canceled)
	fs = newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/dir/file.txt", nil)
	budget = &searchV2Budget{directoriesVisited: maxSearchV2Directories - 1, readRemaining: 1024}
	_, err = runSearchV2(context.Background(), fs, "/", searchV2Payload{PerPage: 100}, budget)
	require.ErrorIs(t, err, errSearchV2TraversalLimit)
	fs = newBetterFilesTestFilesystem(t, 0)
	writeBetterFilesFixture(t, fs, "/"+strings.Repeat("d/", maxSearchV2Depth+1)+"file.txt", nil)
	_, err = runSearchV2(context.Background(), fs, "/", searchV2Payload{PerPage: 100}, nil)
	require.ErrorIs(t, err, errSearchV2TraversalLimit)
}

func TestLegacyGETSearchStillUsesLegacyResponse(t *testing.T) {
	_, s, handler := newBetterFilesHTTPServer(t, 10, 10)
	writeBetterFilesFixture(t, s.Filesystem(), "/config/paper.yml", []byte("legacy"))
	request := httptest.NewRequest(http.MethodGet, "/api/servers/"+s.ID()+"/files/search?"+url.Values{"query": {"paper"}}.Encode(), nil)
	request.Header.Set("Authorization", "Bearer "+config.Get().Token.Token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var legacy FileSearchResponse
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &legacy))
	require.Len(t, legacy.Data, 1)
	require.Equal(t, "paper.yml", legacy.Data[0].Name)
	require.Equal(t, "paper", legacy.Meta.Query)
}
