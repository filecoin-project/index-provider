package engine

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	provider "github.com/filecoin-project/index-provider"
	"github.com/filecoin-project/index-provider/config"
	"github.com/filecoin-project/index-provider/engine/chunker"
	"github.com/filecoin-project/index-provider/metadata"
	"github.com/filecoin-project/index-provider/testutil"
	"github.com/filecoin-project/storetheindex/api/v0/ingest/schema"
	"github.com/ipfs/go-cid"
	"github.com/ipld/go-car/v2/index"
	"github.com/ipld/go-ipld-prime"
	cidlink "github.com/ipld/go-ipld-prime/linking/cid"
	"github.com/ipld/go-ipld-prime/multicodec"
	"github.com/stretchr/testify/require"
)

func Test_EvictedCachedEntriesChainIsRegeneratedGracefully(t *testing.T) {
	rng := rand.New(rand.NewSource(1413))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg := config.NewIngest()
	cfg.LinkedChunkSize = 2
	cfg.LinkCacheSize = 1
	subject := mkEngineWithConfig(t, cfg)

	ad1CtxID := []byte("first")
	ad1MhCount := 12
	wantAd1EntriesChainLen := ad1MhCount / cfg.LinkedChunkSize
	ad1Mhs, err := testutil.RandomCids(rng, ad1MhCount)
	require.NoError(t, err)

	ad2CtxID := []byte("second")
	ad2MhCount := 10
	wantAd2ChunkLen := ad2MhCount / cfg.LinkedChunkSize
	ad2Mhs, err := testutil.RandomCids(rng, ad2MhCount)
	require.NoError(t, err)

	subject.RegisterCallback(func(ctx context.Context, contextID []byte) (provider.MultihashIterator, error) {
		strCtxID := string(contextID)
		if strCtxID == string(ad1CtxID) {
			return getMhIterator(t, ad1Mhs), nil
		}
		if strCtxID == string(ad2CtxID) {
			return getMhIterator(t, ad2Mhs), nil
		}
		return nil, errors.New("not found")
	})

	ad1Cid, err := subject.NotifyPut(ctx, ad1CtxID, metadata.BitswapMetadata)
	require.NoError(t, err)
	ad1, err := subject.GetAdv(ctx, ad1Cid)
	require.NoError(t, err)
	ad1EntriesRoot := requireAdEntriesLink(t, ad1)
	ad1EntriesChain := listEntriesChainFromCache(t, subject.entriesChunker, ad1EntriesRoot)
	require.Len(t, ad1EntriesChain, wantAd1EntriesChainLen)
	requireChunkIsCached(t, subject.entriesChunker, ad1EntriesChain...)
	a1Chunks := requireLoadEntryChunkFromEngine(t, subject, ad1EntriesChain...)

	ad2Cid, err := subject.NotifyPut(ctx, ad2CtxID, metadata.BitswapMetadata)
	require.NoError(t, err)
	ad2, err := subject.GetAdv(ctx, ad2Cid)
	require.NoError(t, err)
	ad2EntriesRoot := requireAdEntriesLink(t, ad2)
	ad2EntriesChain := listEntriesChainFromCache(t, subject.entriesChunker, ad2EntriesRoot)
	require.Len(t, ad2EntriesChain, wantAd2ChunkLen)
	requireChunkIsCached(t, subject.entriesChunker, ad2EntriesChain...)
	a2Chunks := requireLoadEntryChunkFromEngine(t, subject, ad2EntriesChain...)

	// Assert ad1 entries chain is evicted since cache capacity is set to 1.
	requireChunkIsNotCached(t, subject.entriesChunker, ad1EntriesChain...)
	a1ChunksAfterReGen := requireLoadEntryChunkFromEngine(t, subject, ad1EntriesChain...)
	require.Equal(t, a1Chunks, a1ChunksAfterReGen)

	// Assert ad2 entries are no longer cached since ad1 entries were re-generated and cached.
	requireChunkIsNotCached(t, subject.entriesChunker, ad2EntriesChain...)
	a2ChunksAfterReGen := requireLoadEntryChunkFromEngine(t, subject, ad2EntriesChain...)
	require.Equal(t, a2Chunks, a2ChunksAfterReGen)
}

func getMhIterator(t *testing.T, cids []cid.Cid) provider.MultihashIterator {
	idx := index.NewMultihashSorted()
	var records []index.Record
	for i, c := range cids {
		records = append(records, index.Record{
			Cid:    c,
			Offset: uint64(i + 1),
		})
	}
	err := idx.Load(records)
	require.NoError(t, err)
	iterator, err := provider.CarMultihashIterator(idx)
	require.NoError(t, err)
	return iterator
}
func requireAdEntriesLink(t *testing.T, ad schema.Advertisement) ipld.Link {
	lnk, err := ad.FieldEntries().AsLink()
	require.NoError(t, err)
	return lnk
}

func listEntriesChainFromCache(t *testing.T, e *chunker.CachedEntriesChunker, root ipld.Link) []ipld.Link {
	next := root
	var links []ipld.Link
	for {
		raw, err := e.GetRawCachedChunk(context.TODO(), next)
		require.NoError(t, err)
		chunk := requireDecodeAsEntryChunk(t, root, raw)
		links = append(links, next)
		if chunk.FieldNext().IsAbsent() || chunk.FieldNext().IsNull() {
			break
		}
		next, err = chunk.FieldNext().AsNode().AsLink()
		require.NoError(t, err)
	}
	return links
}

func requireLoadEntryChunkFromEngine(t *testing.T, e *Engine, l ...ipld.Link) []schema.EntryChunk {
	var chunks []schema.EntryChunk
	for _, link := range l {
		n, err := e.lsys.Load(ipld.LinkContext{}, link, schema.Type.EntryChunk)
		require.NoError(t, err)
		chunk, ok := n.(schema.EntryChunk)
		require.True(t, ok)
		chunks = append(chunks, chunk)
	}
	return chunks
}

func requireDecodeAsEntryChunk(t *testing.T, l ipld.Link, value []byte) schema.EntryChunk {
	c := l.(cidlink.Link).Cid
	nb := schema.Type.EntryChunk.NewBuilder()
	decoder, err := multicodec.LookupDecoder(c.Prefix().Codec)
	require.NoError(t, err)

	err = decoder(nb, bytes.NewBuffer(value))
	require.NoError(t, err)
	ec, ok := nb.Build().(schema.EntryChunk)
	require.True(t, ok)
	return ec
}

func requireChunkIsCached(t *testing.T, e *chunker.CachedEntriesChunker, l ...ipld.Link) {
	for _, link := range l {
		chunk, err := e.GetRawCachedChunk(context.TODO(), link)
		require.NoError(t, err)
		require.NotEmpty(t, chunk)
	}
}

func requireChunkIsNotCached(t *testing.T, e *chunker.CachedEntriesChunker, l ...ipld.Link) {
	for _, link := range l {
		chunk, err := e.GetRawCachedChunk(context.TODO(), link)
		require.NoError(t, err)
		require.Empty(t, chunk)
	}
}
