package v2

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/grafana/tempo/pkg/model"
	"github.com/grafana/tempo/pkg/model/decoder"
	"github.com/grafana/tempo/pkg/tempopb"
	"github.com/grafana/tempo/pkg/traceql"
	"github.com/grafana/tempo/pkg/warnings"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/encoding/common"
)

const maxDataEncodingLength = 32

var _ common.WALBlock = (*v2AppendBlock)(nil)

// v2AppendBlock is a block that is actively used to append new objects to.  It stores all data in the appendFile
// in the order it was received and an in memory sorted index.
type v2AppendBlock struct {
	meta           *backend.BlockMeta
	ingestionSlack time.Duration

	appendFile *os.File
	appender   Appender

	filepath string
	readFile *os.File
	once     sync.Once
}

func newAppendBlock(id uuid.UUID, tenantID string, filepath string, e backend.Encoding, dataEncoding string, ingestionSlack time.Duration) (common.WALBlock, error) {
	if strings.ContainsRune(dataEncoding, ':') ||
		len([]rune(dataEncoding)) > maxDataEncodingLength {
		return nil, fmt.Errorf("dataEncoding %s is invalid", dataEncoding)
	}

	h := &v2AppendBlock{
		meta:           backend.NewBlockMeta(tenantID, id, VersionString, e, dataEncoding),
		filepath:       filepath,
		ingestionSlack: ingestionSlack,
	}

	name := h.fullFilename()

	f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return nil, err
	}
	h.appendFile = f

	dataWriter, err := NewDataWriter(f, e)
	if err != nil {
		return nil, err
	}

	h.appender = NewAppender(dataWriter)

	return h, nil
}

// newAppendBlockFromFile returns an AppendBlock that can not be appended to, but can
// be completed. It can return a warning or a fatal error
func newAppendBlockFromFile(filename string, path string, ingestionSlack time.Duration, additionalStartSlack time.Duration) (common.WALBlock, error, error) {
	var warning error
	blockID, tenantID, version, e, dataEncoding, err := ParseFilename(filename)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing wal filename: %w", err)
	}

	b := &v2AppendBlock{
		meta:           backend.NewBlockMeta(tenantID, blockID, version, e, dataEncoding),
		filepath:       path,
		ingestionSlack: ingestionSlack,
	}

	// replay file to extract records
	f, err := b.file()
	if err != nil {
		return nil, nil, fmt.Errorf("accessing file: %w", err)
	}

	blockStart := uint32(math.MaxUint32)
	blockEnd := uint32(0)

	dec, err := model.NewObjectDecoder(dataEncoding)
	if err != nil {
		return nil, nil, fmt.Errorf("creating object decoder: %w", err)
	}

	records, warning, err := ReplayWALAndGetRecords(f, e, func(bytes []byte) error {
		start, end, err := dec.FastRange(bytes)
		if err == decoder.ErrUnsupported {
			now := uint32(time.Now().Unix())
			start = now
			end = now
		}
		if err != nil {
			return err
		}

		start, end = b.adjustTimeRangeForSlack(start, end, additionalStartSlack)
		if start < blockStart {
			blockStart = start
		}
		if end > blockEnd {
			blockEnd = end
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	b.appender = NewRecordAppender(records)
	b.meta.TotalObjects = b.appender.Length()
	b.meta.StartTime = time.Unix(int64(blockStart), 0)
	b.meta.EndTime = time.Unix(int64(blockEnd), 0)

	return b, warning, nil
}

// Append adds an id and object to this wal block. start/end should indicate the time range
// associated with the past object. They are unix epoch seconds.
func (a *v2AppendBlock) Append(id common.ID, b []byte, start, end uint32) error {
	err := a.appender.Append(id, b)
	if err != nil {
		return err
	}
	start, end = a.adjustTimeRangeForSlack(start, end, 0)
	a.meta.ObjectAdded(id, start, end)
	return nil
}

func (a *v2AppendBlock) DataLength() uint64 {
	return a.appender.DataLength()
}

func (a *v2AppendBlock) Length() int {
	return a.appender.Length()
}

func (a *v2AppendBlock) BlockMeta() *backend.BlockMeta {
	return a.meta
}

// Iterator returns a common.Iterator that is secretly also a BytesIterator for use internally
func (a *v2AppendBlock) Iterator() (common.Iterator, error) {
	combiner := model.StaticCombiner

	if a.appendFile != nil {
		err := a.appendFile.Close()
		if err != nil {
			return nil, err
		}
		a.appendFile = nil
	}

	records := a.appender.Records()
	readFile, err := a.file()
	if err != nil {
		return nil, err
	}

	dataReader, err := NewDataReader(backend.NewContextReaderWithAllReader(readFile), a.meta.Encoding)
	if err != nil {
		return nil, err
	}

	iterator := newRecordIterator(records, dataReader, NewObjectReaderWriter())
	iterator, err = NewDedupingIterator(iterator, combiner, a.meta.DataEncoding)
	if err != nil {
		return nil, err
	}

	dec, err := model.NewObjectDecoder(a.meta.DataEncoding)
	if err != nil {
		return nil, fmt.Errorf("creating object decoder: %w", err)
	}

	return &commonIterator{
		iter: iterator,
		dec:  dec,
	}, nil
}

func (a *v2AppendBlock) Clear() error {
	if a.readFile != nil {
		_ = a.readFile.Close()
		a.readFile = nil
	}

	if a.appendFile != nil {
		_ = a.appendFile.Close()
		a.appendFile = nil
	}

	// ignore error, it's important to remove the file above all else
	_ = a.appender.Complete()

	name := a.fullFilename()
	return os.Remove(name)
}

// Find implements common.Finder
func (a *v2AppendBlock) FindTraceByID(ctx context.Context, id common.ID, opts common.SearchOptions) (*tempopb.Trace, error) {
	combiner := model.StaticCombiner

	records := a.appender.RecordsForID(id)
	file, err := a.file()
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, nil
	}

	dataReader, err := NewDataReader(backend.NewContextReaderWithAllReader(file), a.meta.Encoding)
	if err != nil {
		return nil, err
	}
	defer dataReader.Close()
	finder := newPagedFinder(Records(records), dataReader, combiner, NewObjectReaderWriter(), a.meta.DataEncoding)

	bytes, err := finder.Find(context.Background(), id)
	if err != nil {
		return nil, err
	}

	if bytes == nil {
		return nil, nil
	}

	dec, err := model.NewObjectDecoder(a.meta.DataEncoding)
	if err != nil {
		return nil, err
	}

	return dec.PrepareForRead(bytes)
}

// Search implements common.Searcher
func (a *v2AppendBlock) Search(ctx context.Context, req *tempopb.SearchRequest, opts common.SearchOptions) (*tempopb.SearchResponse, error) {
	return nil, common.ErrUnsupported
}

// Search implements common.Searcher
func (a *v2AppendBlock) SearchTags(ctx context.Context, cb common.TagCallback, opts common.SearchOptions) error {
	return common.ErrUnsupported
}

// SearchTagValues implements common.Searcher
func (a *v2AppendBlock) SearchTagValues(ctx context.Context, tag string, cb common.TagCallback, opts common.SearchOptions) error {
	return common.ErrUnsupported
}

// Fetch implements traceql.SpansetFetcher
func (a *v2AppendBlock) Fetch(context.Context, traceql.FetchSpansRequest) (traceql.FetchSpansResponse, error) {
	return traceql.FetchSpansResponse{}, common.ErrUnsupported
}

func (a *v2AppendBlock) fullFilename() string {
	if a.meta.Version == "v0" {
		return filepath.Join(a.filepath, fmt.Sprintf("%v:%v", a.meta.BlockID, a.meta.TenantID))
	}

	var filename string
	if a.meta.DataEncoding == "" {
		filename = fmt.Sprintf("%v:%v:%v:%v", a.meta.BlockID, a.meta.TenantID, a.meta.Version, a.meta.Encoding)
	} else {
		filename = fmt.Sprintf("%v:%v:%v:%v:%v", a.meta.BlockID, a.meta.TenantID, a.meta.Version, a.meta.Encoding, a.meta.DataEncoding)
	}

	return filepath.Join(a.filepath, filename)
}

func (a *v2AppendBlock) file() (*os.File, error) {
	var err error
	a.once.Do(func() {
		if a.readFile == nil {
			name := a.fullFilename()

			a.readFile, err = os.OpenFile(name, os.O_RDONLY, 0644)
		}
	})

	return a.readFile, err
}

func (a *v2AppendBlock) adjustTimeRangeForSlack(start uint32, end uint32, additionalStartSlack time.Duration) (uint32, uint32) {
	now := time.Now()
	startOfRange := uint32(now.Add(-a.ingestionSlack).Add(-additionalStartSlack).Unix())
	endOfRange := uint32(now.Add(a.ingestionSlack).Unix())

	warn := false
	if start < startOfRange {
		warn = true
		start = uint32(now.Unix())
	}
	if end > endOfRange {
		warn = true
		end = uint32(now.Unix())
	}

	if warn {
		warnings.Metric.WithLabelValues(a.meta.TenantID, warnings.ReasonOutsideIngestionSlack).Inc()
	}

	return start, end
}

// ParseFilename returns (blockID, tenant, version, encoding, dataEncoding, error).
// Example: "00000000-0000-0000-0000-000000000000:1:v2:snappy:v1"
func ParseFilename(filename string) (uuid.UUID, string, string, backend.Encoding, string, error) {
	splits := strings.Split(filename, ":")

	if len(splits) != 4 && len(splits) != 5 {
		return uuid.UUID{}, "", "", backend.EncNone, "", fmt.Errorf("unable to parse %s. unexpected number of segments", filename)
	}

	// first segment is blockID
	id, err := uuid.Parse(splits[0])
	if err != nil {
		return uuid.UUID{}, "", "", backend.EncNone, "", fmt.Errorf("unable to parse %s. error parsing uuid: %w", filename, err)
	}

	// second segment is tenant
	tenant := splits[1]
	if len(tenant) == 0 {
		return uuid.UUID{}, "", "", backend.EncNone, "", fmt.Errorf("unable to parse %s. missing fields", filename)
	}

	// third segment is version
	version := splits[2]
	if version != VersionString {
		return uuid.UUID{}, "", "", backend.EncNone, "", fmt.Errorf("unable to parse %s. error parsing version: %w", filename, err)
	}

	// fourth is encoding
	encodingString := splits[3]
	encoding, err := backend.ParseEncoding(encodingString)
	if err != nil {
		return uuid.UUID{}, "", "", backend.EncNone, "", fmt.Errorf("unable to parse %s. error parsing encoding: %w", filename, err)
	}

	// fifth is dataEncoding
	dataEncoding := ""
	if len(splits) == 5 {
		dataEncoding = splits[4]
	}

	return id, tenant, version, encoding, dataEncoding, nil
}

var _ BytesIterator = (*commonIterator)(nil)
var _ common.Iterator = (*commonIterator)(nil)

// commonIterator implements both BytesIterator and common.Iterator. it is returned from the AppendFile and is meant
// to be passed to a CreateBlock
type commonIterator struct {
	iter BytesIterator
	dec  model.ObjectDecoder
}

func (i *commonIterator) Next(ctx context.Context) (common.ID, *tempopb.Trace, error) {
	id, obj, err := i.iter.NextBytes(ctx)
	if err != nil || obj == nil {
		return id, nil, err
	}

	tr, err := i.dec.PrepareForRead(obj)
	if err != nil {
		return nil, nil, err
	}

	return id, tr, nil
}

func (i *commonIterator) NextBytes(ctx context.Context) (common.ID, []byte, error) {
	return i.iter.NextBytes(ctx)
}

func (i *commonIterator) Close() {
	i.iter.Close()
}
