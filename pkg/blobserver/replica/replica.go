/*
Copyright 2011 Google Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/*
Package replica registers the "replica" blobserver storage type,
providing synchronous replication to one more backends.

Writes wait for minWritesForSuccess (default: all). Reads are
attempted in order and not load-balanced, randomized, or raced by
default.

Example config:

      "/repl/": {
          "handler": "storage-replica",
          "handlerArgs": {
              "backends": ["/b1/", "/b2/", "/b3/"],
              "minWritesForSuccess": 2
          }
      },
*/
package replica

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"camlistore.org/pkg/blobref"
	"camlistore.org/pkg/blobserver"
	"camlistore.org/pkg/jsonconfig"
)

const buffered = 8

type replicaStorage struct {
	*blobserver.SimpleBlobHubPartitionMap

	replicaPrefixes []string
	replicas        []blobserver.Storage

	// Minimum number of writes that must succeed before
	// acknowledging success to the client.
	minWritesForSuccess int

	ctx *http.Request // optional per-request context
}

func (sto *replicaStorage) GetBlobHub() blobserver.BlobHub {
	return sto.SimpleBlobHubPartitionMap.GetBlobHub()
}

var _ blobserver.ContextWrapper = (*replicaStorage)(nil)

func (sto *replicaStorage) WrapContext(req *http.Request) blobserver.Storage {
	s2 := new(replicaStorage)
	*s2 = *sto
	s2.ctx = req
	return s2
}

func (sto *replicaStorage) wrappedReplicas() []blobserver.Storage {
	if sto.ctx == nil {
		return sto.replicas
	}
	w := make([]blobserver.Storage, len(sto.replicas))
	for i, r := range sto.replicas {
		w[i] = blobserver.MaybeWrapContext(r, sto.ctx)
	}
	return w
}

func newFromConfig(ld blobserver.Loader, config jsonconfig.Obj) (storage blobserver.Storage, err error) {
	sto := &replicaStorage{
		SimpleBlobHubPartitionMap: &blobserver.SimpleBlobHubPartitionMap{},
	}
	sto.replicaPrefixes = config.RequiredList("backends")
	nReplicas := len(sto.replicaPrefixes)
	sto.minWritesForSuccess = config.OptionalInt("minWritesForSuccess", nReplicas)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if nReplicas == 0 {
		return nil, errors.New("replica: need at least one replica")
	}
	if sto.minWritesForSuccess == 0 {
		sto.minWritesForSuccess = nReplicas
	}
	sto.replicas = make([]blobserver.Storage, nReplicas)
	for i, prefix := range sto.replicaPrefixes {
		replicaSto, err := ld.GetStorage(prefix)
		if err != nil {
			return nil, err
		}
		sto.replicas[i] = replicaSto
	}
	return sto, nil
}

func (sto *replicaStorage) FetchStreaming(b *blobref.BlobRef) (file io.ReadCloser, size int64, err error) {
	for _, replica := range sto.replicas {
		file, size, err = replica.FetchStreaming(b)
		if err == nil {
			return
		}
	}
	return
}

func (sto *replicaStorage) StatBlobs(dest chan<- blobref.SizedBlobRef, blobs []*blobref.BlobRef, wait time.Duration) error {
	if wait > 0 {
		// TODO: handle wait in-memory, waiting on the blobhub, not going
		// to the replicas?
	}

	need := make(map[string]*blobref.BlobRef)
	for _, br := range blobs {
		need[br.String()] = br
	}

	ch := make(chan blobref.SizedBlobRef, buffered)
	donechan := make(chan bool)

	go func() {
		for sb := range ch {
			bstr := sb.BlobRef.String()
			if _, needed := need[bstr]; needed {
				dest <- sb
				delete(need, bstr)
			}
		}
		donechan <- true
	}()

	errch := make(chan error, buffered)
	statReplica := func(s blobserver.Storage) {
		errch <- s.StatBlobs(ch, blobs, wait)
	}

	for _, replica := range sto.wrappedReplicas() {
		go statReplica(replica)
	}

	var retErr error
	for _ = range sto.replicas {
		if err := <-errch; err != nil {
			retErr = err
		}
	}
	close(ch)
	<-donechan

	// Safe to access need map now; as helper goroutine is
	// done with it.
	if len(need) == 0 {
		return nil
	}
	return retErr
}

type sizedBlobAndError struct {
	sb  blobref.SizedBlobRef
	err error
}

func (sto *replicaStorage) ReceiveBlob(b *blobref.BlobRef, source io.Reader) (_ blobref.SizedBlobRef, err error) {
	nReplicas := len(sto.replicas)
	rpipe, wpipe, writer := make([]*io.PipeReader, nReplicas), make([]*io.PipeWriter, nReplicas), make([]io.Writer, nReplicas)
	for idx := range sto.replicas {
		rpipe[idx], wpipe[idx] = io.Pipe()
		writer[idx] = wpipe[idx]
		// TODO: deal with slow/hung clients. this scheme of pipes +
		// multiwriter (even with a bufio.Writer thrown in) isn't
		// sufficient to guarantee forward progress. perhaps something
		// like &MoveOrDieWriter{Writer: wpipe[idx], HeartbeatSec: 10}
	}
	upResult := make(chan sizedBlobAndError, nReplicas)
	uploadToReplica := func(source io.Reader, s blobserver.Storage) {
		s = blobserver.MaybeWrapContext(s, sto.ctx)
		sb, err := s.ReceiveBlob(b, source)
		if err != nil {
			io.Copy(ioutil.Discard, source)
		}
		upResult <- sizedBlobAndError{sb, err}
	}
	for idx, replica := range sto.wrappedReplicas() {
		go uploadToReplica(rpipe[idx], replica)
	}
	size, err := io.Copy(io.MultiWriter(writer...), source)
	if err != nil {
		return
	}
	for idx := range sto.replicas {
		wpipe[idx].Close()
	}
	nSuccess, nFailures := 0, 0
	for _ = range sto.replicas {
		res := <-upResult
		switch {
		case res.err == nil && res.sb.Size == size:
			nSuccess++
			if nSuccess == sto.minWritesForSuccess {
				sto.GetBlobHub().NotifyBlobReceived(b)
				return res.sb, nil
			}
		case res.err == nil:
			nFailures++
			err = fmt.Errorf("replica: upload shard reported size %d, expected %d", res.sb.Size, size)
		default:
			nFailures++
			err = res.err
		}
	}
	if nFailures > 0 {
		log.Printf("replica: receiving blob, %d successes, %d failures; last error = %v",
			nSuccess, nFailures, err)
	}
	return
}

func (sto *replicaStorage) RemoveBlobs(blobs []*blobref.BlobRef) error {
	errch := make(chan error, buffered)
	removeFrom := func(s blobserver.Storage) {
		s = blobserver.MaybeWrapContext(s, sto.ctx)
		errch <- s.RemoveBlobs(blobs)
	}
	for _, replica := range sto.replicas {
		go removeFrom(replica)
	}
	var reterr error
	nSuccess := 0
	for _ = range errch {
		if err := <-errch; err != nil {
			reterr = err
		} else {
			nSuccess++
		}
	}
	if nSuccess > 0 {
		// TODO: decide on the return value. for now this is best
		// effort and we return nil if any of the blobservers said
		// success.  maybe a bit weird, though.
		return nil
	}
	return reterr
}

func (sto *replicaStorage) EnumerateBlobs(dest chan<- blobref.SizedBlobRef, after string, limit int, wait time.Duration) error {
	// TODO: option to enumerate from one or from all merged.  for
	// now we'll just do all, even though it's kinda a waste.  at
	// least then we don't miss anything if a certain node is
	// missing some blobs temporarily
	return blobserver.MergedEnumerate(dest, sto.wrappedReplicas(), after, limit, wait)
}

func init() {
	blobserver.RegisterStorageConstructor("replica", blobserver.StorageConstructor(newFromConfig))
}
