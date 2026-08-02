package router

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"

	"github.com/pterodactyl/wings/internal/ufs"
	"github.com/pterodactyl/wings/router/middleware"
	serverfs "github.com/pterodactyl/wings/server/filesystem"
)

const maxFingerprintFiles = 100

type fingerprintJob struct {
	key  string
	path string
}

type fingerprintResult struct {
	key   string
	value string
	valid bool
}

func getServerFileFingerprints(c *gin.Context) {
	s := middleware.ExtractServer(c)
	algorithm := c.Query("algorithm")
	if !validFingerprintAlgorithm(algorithm) {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Unsupported fingerprint algorithm."})
		return
	}
	requested := c.QueryArray("files")
	if len(requested) == 0 {
		requested = c.QueryArray("files[]")
	}
	if len(requested) == 0 || len(requested) > maxFingerprintFiles {
		c.AbortWithStatusJSON(http.StatusUnprocessableEntity, gin.H{"error": "Between 1 and 100 files are required."})
		return
	}
	root, err := normalizeBetterFilesRoot(c.Query("root"))
	if err != nil || ensureBetterFilesAllowed(s.Filesystem(), root) != nil {
		abortBetterFilesPath(c, errBetterFilesInvalidPath)
		return
	}

	jobs := make([]fingerprintJob, 0, len(requested))
	for _, key := range requested {
		filePath, err := joinBetterFilesPath(root, key)
		if err != nil {
			abortBetterFilesPath(c, err)
			return
		}
		if ensureBetterFilesAllowed(s.Filesystem(), filePath) != nil {
			continue
		}
		jobs = append(jobs, fingerprintJob{key: key, path: filePath})
	}
	fingerprints := fingerprintFiles(c.Request.Context(), s.Filesystem(), algorithm, jobs)
	c.JSON(http.StatusOK, gin.H{"fingerprints": fingerprints})
}

func validFingerprintAlgorithm(value string) bool {
	switch value {
	case "md5", "crc32", "sha1", "sha224", "sha256", "sha384", "sha512", "curseforge":
		return true
	default:
		return false
	}
}

func fingerprintFiles(ctx context.Context, fs *serverfs.Filesystem, algorithm string, jobs []fingerprintJob) map[string]string {
	workerCount := runtime.NumCPU()
	if workerCount < 1 {
		workerCount = 1
	}
	if workerCount > 8 {
		workerCount = 8
	}
	if workerCount > len(jobs) {
		workerCount = len(jobs)
	}
	if workerCount == 0 {
		return map[string]string{}
	}

	jobChannel := make(chan fingerprintJob)
	resultChannel := make(chan fingerprintResult, len(jobs))
	var workers sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			buffer := make([]byte, 64*1024)
			for job := range jobChannel {
				value, err := fingerprintOneFile(ctx, fs, algorithm, job.path, buffer)
				select {
				case resultChannel <- fingerprintResult{key: job.key, value: value, valid: err == nil}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobChannel)
		for _, job := range jobs {
			select {
			case jobChannel <- job:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		workers.Wait()
		close(resultChannel)
	}()

	result := make(map[string]string, len(jobs))
	for fingerprint := range resultChannel {
		if fingerprint.valid {
			result[fingerprint.key] = fingerprint.value
		}
	}
	return result
}

func fingerprintOneFile(ctx context.Context, fs *serverfs.Filesystem, algorithm, filePath string, buffer []byte) (string, error) {
	info, err := betterFilesLstat(fs, filePath)
	if err != nil || !info.Mode().IsRegular() {
		return "", errBetterFilesInvalidPath
	}
	file, err := fs.UnixFS().OpenFile(filePath, ufs.O_RDONLY|ufs.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer file.Close()
	if algorithm == "curseforge" {
		return curseForgeFingerprint(ctx, file, buffer)
	}

	hasher, crc, err := newFingerprintHasher(algorithm)
	if err != nil {
		return "", err
	}
	reader := &fingerprintContextReader{ctx: ctx, reader: file}
	if _, err := io.CopyBuffer(hasher, reader, buffer); err != nil {
		return "", err
	}
	if crc != nil {
		return fmt.Sprintf("%08x", crc.Sum32()), nil
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func newFingerprintHasher(algorithm string) (hash.Hash, hash.Hash32, error) {
	switch algorithm {
	case "md5":
		return md5.New(), nil, nil
	case "crc32":
		hasher := crc32.NewIEEE()
		return hasher, hasher, nil
	case "sha1":
		return sha1.New(), nil, nil
	case "sha224":
		return sha256.New224(), nil, nil
	case "sha256":
		return sha256.New(), nil, nil
	case "sha384":
		return sha512.New384(), nil, nil
	case "sha512":
		return sha512.New(), nil, nil
	default:
		return nil, nil, fmt.Errorf("unsupported fingerprint algorithm")
	}
}

func curseForgeFingerprint(ctx context.Context, file ufs.File, buffer []byte) (string, error) {
	var normalizedLength uint32
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		for _, value := range buffer[:n] {
			if !curseForgeIgnoredByte(value) {
				normalizedLength++
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	const multiplier uint32 = 1540483477
	hashValue := uint32(1) ^ normalizedLength
	var tail uint32
	var shift uint32
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		for _, value := range buffer[:n] {
			if curseForgeIgnoredByte(value) {
				continue
			}
			tail |= uint32(value) << shift
			shift += 8
			if shift == 32 {
				mixed := tail * multiplier
				mixed = (mixed ^ (mixed >> 24)) * multiplier
				hashValue = hashValue*multiplier ^ mixed
				tail = 0
				shift = 0
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	if shift > 0 {
		hashValue = (hashValue ^ tail) * multiplier
	}
	hashValue = (hashValue ^ (hashValue >> 13)) * multiplier
	hashValue ^= hashValue >> 15
	return strconv.FormatUint(uint64(hashValue), 10), nil
}

func curseForgeIgnoredByte(value byte) bool {
	return value == '\t' || value == '\n' || value == '\r' || value == ' '
}

type fingerprintContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *fingerprintContextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}
