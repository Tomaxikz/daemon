package downloader

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDownloadJSON(t *testing.T) {
	download := &Download{Identifier: "download-id", progress: 0.25}
	data, err := json.Marshal(download)
	require.NoError(t, err)
	require.JSONEq(t, `{"Identifier":"download-id","Progress":0.25}`, string(data))
	data, err = json.Marshal([]*Download{download})
	require.NoError(t, err)
	require.JSONEq(t, `[{"Identifier":"download-id","Progress":0.25}]`, string(data))
}

func TestDownloadJSONConcurrent(t *testing.T) {
	download := &Download{Identifier: "download-id"}
	counter := download.counter(100)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()

		for i := 0; i < 100; i++ {
			if _, err := counter.Write([]byte{0}); err != nil {
				t.Errorf("update progress: %v", err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()

		for i := 0; i < 100; i++ {
			data, err := json.Marshal(download)
			if err != nil {
				t.Errorf("marshal download: %v", err)
				return
			}

			var value struct {
				Identifier string
				Progress   float64
			}
			if err := json.Unmarshal(data, &value); err != nil {
				t.Errorf("decode download: %v", err)
				return
			}
			if value.Identifier != "download-id" || value.Progress < 0 || value.Progress > 1 {
				t.Error("invalid download snapshot")
				return
			}
		}
	}()
	wg.Wait()
	require.Equal(t, float64(1), download.Progress())
}
