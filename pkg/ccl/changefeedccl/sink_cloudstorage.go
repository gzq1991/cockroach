// Copyright 2019 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package changefeedccl

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"sync/atomic"

	"github.com/cockroachdb/cockroach/pkg/ccl/storageccl"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/pkg/errors"
)

func isCloudStorageSink(u *url.URL) bool {
	switch u.Scheme {
	case `experimental-s3`, `experimental-gs`, `experimental-nodelocal`, `experimental-http`,
		`experimental-https`, `experimental-azure`:
		return true
	default:
		return false
	}
}

// cloudStorageFormatTime formats times as YYYYMMDDHHMMSSNNNNNNNNNLLLLLLLLLL.
func cloudStorageFormatTime(ts hlc.Timestamp) string {
	// TODO(dan): This is an absurdly long way to print out this timestamp, but
	// I kept hitting bugs while trying to do something clever to make it
	// shorter. Revisit.
	const f = `20060102150405`
	t := ts.GoTime()
	return fmt.Sprintf(`%s%09d%010d`, t.Format(f), t.Nanosecond(), ts.Logical)
}

type cloudStorageSinkKey struct {
	Topic    string
	SchemaID sqlbase.DescriptorVersion
}

type cloudStorageSinkFile struct {
	leastResolvedTs hlc.Timestamp
	buf             bytes.Buffer
}

// cloudStorageSink emits to files on cloud storage.
//
// The data files written by this sink are named according to the pattern
// `<timestamp>_<topic>_<schema_id>_<uniquer>.<ext>`, each component of which is
// as follows:
//
// `<timestamp>` is the smallest timestamp of any entries in the file.
//
// `<topic>` corresponds to one SQL table.
//
// `<schema_id>` changes whenever the SQL table schema changes, which allows us
// to guarantee to users that _all entries in a given file have the same
// schema_.
//
// `<uniquer>` is used to keep nodes in a cluster from overwriting each other's
// data and should be ignored by external users. It also keeps a single node
// from overwriting its own data if there are multiple changefeeds, or if a
// changefeed gets canceled/restarted/zombied. Internally, it's generated by
// `<node_id>-<sink_id>-<file_id>` where `<sink_id>` is a unique id for each
// cloudStorageSink in a running process and `<file_id>` is a unique id for each
// file written by a given `<sink_id>`.
//
// `<ext>` implies the format of the file: currently the only option is
// `ndjson`, which means a text file conforming to the "Newline Delimited JSON"
// spec.
//
// Each record in the data files is a value, keys are not included, so the
// `envelope` option must be set to `value_only`. Within a file, records are not
// guaranteed to be sorted by timestamp. A duplicate of some records might exist
// in a different file or even in the same file.
//
// The resolved timestamp files are named `<timestamp>.RESOLVED`. This is
// carefully done so that we can offer the following external guarantee: At any
// given time, if the the files are iterated in lexicographic filename order,
// then encountering any filename containing `RESOLVED` means that everything
// before it is finalized (and thus can be ingested into some other system and
// deleted, included in hive queries, etc). A typical user of cloudStorageSink
// would periodically do exactly this.
//
// Still TODO is writing out data schemas, Avro support, bounding memory usage.
type cloudStorageSink struct {
	nodeID            roachpb.NodeID
	sinkID            int64
	targetMaxFileSize int64
	settings          *cluster.Settings
	partitionFormat   string

	ext           string
	recordDelimFn func(io.Writer) error

	es               storageccl.ExportStorage
	fileID           int64
	files            map[cloudStorageSinkKey]*cloudStorageSinkFile
	sf               *spanFrontier
	initialHighWater hlc.Timestamp
	jobSessionID     string
}

var cloudStorageSinkIDAtomic int64

func makeCloudStorageSink(
	baseURI string,
	nodeID roachpb.NodeID,
	sessionID string,
	targetMaxFileSize int64,
	settings *cluster.Settings,
	opts map[string]string,
	watchedSF *spanFrontier,
	initialHighWater hlc.Timestamp,
) (Sink, error) {
	// Date partitioning is pretty standard, so no override for now, but we could
	// plumb one down if someone needs it.
	const defaultPartitionFormat = `2006-01-02`

	sinkID := atomic.AddInt64(&cloudStorageSinkIDAtomic, 1)
	s := &cloudStorageSink{
		nodeID:            nodeID,
		sinkID:            sinkID,
		settings:          settings,
		targetMaxFileSize: targetMaxFileSize,
		files:             make(map[cloudStorageSinkKey]*cloudStorageSinkFile),
		partitionFormat:   defaultPartitionFormat,
		sf:                watchedSF,
		initialHighWater:  initialHighWater,
		jobSessionID:      sessionID,
	}

	switch formatType(opts[optFormat]) {
	case optFormatJSON:
		// TODO(dan): It seems like these should be on the encoder, but that
		// would require a bit of refactoring.
		s.ext = `.ndjson`
		s.recordDelimFn = func(w io.Writer) error {
			_, err := w.Write([]byte{'\n'})
			return err
		}
	default:
		return nil, errors.Errorf(`this sink is incompatible with %s=%s`,
			optFormat, opts[optFormat])
	}

	switch envelopeType(opts[optEnvelope]) {
	case optEnvelopeWrapped:
	default:
		return nil, errors.Errorf(`this sink is incompatible with %s=%s`,
			optEnvelope, opts[optEnvelope])
	}

	if _, ok := opts[optKeyInValue]; !ok {
		return nil, errors.Errorf(`this sink requires the WITH %s option`, optKeyInValue)
	}

	ctx := context.TODO()
	var err error
	if s.es, err = storageccl.ExportStorageFromURI(ctx, baseURI, settings); err != nil {
		return nil, err
	}

	return s, nil
}

// EmitRow implements the Sink interface.
func (s *cloudStorageSink) EmitRow(
	ctx context.Context, table *sqlbase.TableDescriptor, _, value []byte, updated hlc.Timestamp,
) error {
	if s.files == nil {
		return errors.New(`cannot EmitRow on a closed sink`)
	}

	key := cloudStorageSinkKey{
		Topic:    table.Name,
		SchemaID: table.Version,
	}
	file := s.files[key]
	if file == nil {
		// We could pool the bytes.Buffers if necessary, but we'd need to be
		// careful to bound the size of the memory held by the pool.
		leastResolvedTs := s.sf.Frontier()
		if leastResolvedTs.Less(s.initialHighWater) {
			// This condition being true indicates that this is a new job that
			// hasn't yet seen any resolved timestamps. So we start with the
			// initialHighWater.
			leastResolvedTs = s.initialHighWater
		}
		file = &cloudStorageSinkFile{leastResolvedTs: leastResolvedTs}
		s.files[key] = file
	}
	if updated.Less(file.leastResolvedTs) {
		// Avoid leakage of old previously seen data into new file.
		return nil
	}

	// TODO(dan): Memory monitoring for this
	if _, err := file.buf.Write(value); err != nil {
		return err
	}
	if err := s.recordDelimFn(&file.buf); err != nil {
		return err
	}

	if int64(file.buf.Len()) > s.targetMaxFileSize {
		if err := s.flushFile(ctx, key, file); err != nil {
			return err
		}
		delete(s.files, key)
	}
	return nil
}

// EmitResolvedTimestamp implements the Sink interface.
func (s *cloudStorageSink) EmitResolvedTimestamp(
	ctx context.Context, encoder Encoder, resolved hlc.Timestamp,
) error {
	if s.files == nil {
		return errors.New(`cannot EmitRow on a closed sink`)
	}

	var noTopic string
	payload, err := encoder.EncodeResolvedTimestamp(noTopic, resolved)
	if err != nil {
		return err
	}
	// Don't need to copy payload because we never buffer it anywhere.

	part := resolved.GoTime().Format(s.partitionFormat)
	filename := fmt.Sprintf(`%s.RESOLVED`, cloudStorageFormatTime(resolved))
	log.Info(ctx, "writing ", filename)
	return s.es.WriteFile(ctx, filepath.Join(part, filename), bytes.NewReader(payload))
}

// Flush implements the Sink interface.
func (s *cloudStorageSink) Flush(ctx context.Context) error {
	if s.files == nil {
		return errors.New(`cannot Flush on a closed sink`)
	}

	for key, file := range s.files {
		if err := s.flushFile(ctx, key, file); err != nil {
			return err
		}
	}
	for key := range s.files {
		delete(s.files, key)
	}
	return nil
}

func (s *cloudStorageSink) flushFile(
	ctx context.Context, key cloudStorageSinkKey, file *cloudStorageSinkFile,
) error {
	if file.buf.Len() == 0 {
		// This method shouldn't be called with an empty file, but be defensive
		// about not writing empty files anyway.
		return nil
	}

	part := file.leastResolvedTs.GoTime().Format(s.partitionFormat)
	ts := cloudStorageFormatTime(file.leastResolvedTs)
	fileID := s.fileID
	s.fileID++
	// Pad file ID to maintain lexical ordering among files from the same sink.
	// Note that we use underscores because we want these files to lexicographically
	// succeed `%d.RESOLVED` files with the same timestamp.
	filename := fmt.Sprintf(`%s_%s_%d_%d_%d_%012d_%s%s`,
		ts, key.Topic, key.SchemaID, s.nodeID, s.sinkID, fileID, s.jobSessionID, s.ext)
	if log.V(1) {
		log.Info(ctx, "writing ", filename)
	}
	return s.es.WriteFile(ctx, filepath.Join(part, filename), bytes.NewReader(file.buf.Bytes()))
}

// Close implements the Sink interface.
func (s *cloudStorageSink) Close() error {
	s.files = nil
	return s.es.Close()
}
