package storage

import (
	"bufio"
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.skia.org/infra/go/testutils/unittest"
	"go.skia.org/infra/golden/go/types"
)

const (
	// hashesGCSPath is the bucket/path combination where the test file will be written.
	hashesGCSPath = "skia-infra-testdata/hash_files/testing-known-hashes.txt"
)

func TestWritingHashes(t *testing.T) {
	unittest.LargeTest(t)
	gsClient, opt := initGSClient(t)

	knownDigests := types.DigestSlice{
		"c003788f8d306ff1226e2a460835dae4",
		"885b31941c25efc313b0fd66d55b86d9",
		"264d0d87b12ba337f796fc592cd5357d",
		"69c2fbf8e89a48058b2f45ad4ea46a35",
		"2c4d605c16e7d5b23294c0433fa3ed17",
		"782717cf6ed9329fc43cb5a6c830cbce",
		"e143ca619f2172d06bb0dcc4d72af414",
		"26aff0619c829bc149f7c0171fcca442",
		"72d61ae8e232c3a279cc3cdbf6ef73e5",
		"f1eb049dac1cfa3c70aac8fc6ad5496f",
	}
	require.NoError(t, gsClient.WriteKnownDigests(knownDigests))
	removePaths := []string{opt.HashesGSPath}
	defer func() {
		for _, path := range removePaths {
			_ = gsClient.RemoveForTestingOnly(path)
		}
	}()

	found := loadKnownHashes(t, gsClient)
	require.Equal(t, knownDigests, found)
}

func initGSClient(t *testing.T) (GCSClient, GCSClientOptions) {
	timeStamp := fmt.Sprintf("%032d", time.Now().UnixNano())
	opt := GCSClientOptions{
		HashesGSPath: hashesGCSPath + "-" + timeStamp,
	}
	gsClient, err := NewGCSClient(nil, opt)
	require.NoError(t, err)
	return gsClient, opt
}

func loadKnownHashes(t *testing.T, gsClient GCSClient) types.DigestSlice {
	var buf bytes.Buffer
	require.NoError(t, gsClient.LoadKnownDigests(&buf))

	scanner := bufio.NewScanner(&buf)
	ret := types.DigestSlice{}
	for scanner.Scan() {
		ret = append(ret, types.Digest(scanner.Text()))
	}
	return ret
}
