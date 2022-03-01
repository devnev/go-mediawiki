package mediawiki

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/cosnicolaou/pbzip2"
	"github.com/hashicorp/go-retryablehttp"
	gzip "github.com/klauspost/pgzip"
	"github.com/pingcap/parser"
	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/test_driver"
	"gitlab.com/tozd/go/errors"
	"gitlab.com/tozd/go/x"
	"golang.org/x/text/unicode/norm"
)

const (
	progressPrintRate = 30 * time.Second
)

type syncVar struct {
	lock *sync.RWMutex
	cond *sync.Cond
	v    interface{}
}

func (w *syncVar) Load() interface{} {
	w.cond.L.Lock()
	defer w.cond.L.Unlock()
	for w.v == nil {
		w.cond.Wait()
	}
	return w.v
}

func (w *syncVar) Store(v interface{}) errors.E {
	w.lock.Lock()
	defer w.lock.Unlock()
	if w.v != nil {
		return errors.New("already stored")
	}
	w.v = v
	w.cond.Broadcast()
	return nil
}

func newWriteOnce() *syncVar {
	l := &sync.RWMutex{}
	return &syncVar{
		lock: l,
		cond: sync.NewCond(l.RLocker()),
	}
}

type iterator interface {
	More() bool
	Next(*[]byte) errors.E
}

type jsonIterator json.Decoder

func (i *jsonIterator) More() bool {
	return (*json.Decoder)(i).More()
}

func (i *jsonIterator) Next(b *[]byte) errors.E {
	err := (*json.Decoder)(i).Decode((*json.RawMessage)(b))
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func newJSONIterator(r io.Reader) iterator { //nolint:ireturn
	return (*jsonIterator)(json.NewDecoder(r))
}

type statementIterator struct {
	reader *bufio.Reader
	buffer bytes.Buffer
}

func (i *statementIterator) More() bool {
	if i.buffer.Len() > 0 {
		return true
	}
	_, err := i.reader.Peek(1)
	return !errors.Is(err, io.EOF)
}

func (i *statementIterator) Next(b *[]byte) errors.E {
	line, err := i.reader.ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && i.buffer.Len() > 0 {
			*b = i.buffer.Bytes()
			i.buffer = bytes.Buffer{}
			return nil
		}
		return errors.WithStack(err)
	}
	if len(bytes.TrimSpace(line)) == 0 || bytes.HasPrefix(line, []byte("--")) {
		return i.Next(b)
	}
	i.buffer.Write(line)
	if !bytes.HasSuffix(line, []byte(";\n")) {
		return i.Next(b)
	}
	*b = i.buffer.Bytes()
	i.buffer = bytes.Buffer{}
	return nil
}

func newStatementIterator(r io.Reader) *statementIterator {
	return &statementIterator{
		reader: bufio.NewReader(r),
		buffer: bytes.Buffer{},
	}
}

type FileType int

const (
	JSONArray FileType = iota
	NDJSON
	SQLDump
)

type Compression int

const (
	NoCompression Compression = iota
	Tar
	BZIP2
	BZIP2Tar
	GZIP
	GZIPTar
)

// ProcessConfig is a configuration for low-level Process function.
//
// URL or Path, Process, Item, FileType, and Compression are required.
// If URL is provided and Path does not already exist, Client is required, too.
//
// If just URL is provided, but not Path, then Process downloads and processes
// the file at URL, but does not save it. If both URL and Path are provided,
// and there file at Path does not exist, then Process saves the file at Path
// while downloading and processing the file at URL. If the file at Path already
// exists, then Process just uses it as-is and does not download anything from URL.
//
// Client should set User-Agent header with contact information, e.g.:
//
//     client := retryablehttp.NewClient()
//     client.RequestLogHook = func(logger retryablehttp.Logger, req *http.Request, retry int) {
//     	req.Header.Set("User-Agent", "My bot (user@example.com)")
//     }
type ProcessConfig struct {
	URL                    string
	Path                   string
	Client                 *retryablehttp.Client
	DecompressionThreads   int
	DecodingThreads        int
	ItemsProcessingThreads int
	Process                func(context.Context, interface{}) errors.E
	Progress               func(context.Context, x.Progress)
	Item                   interface{}
	FileType               FileType
	Compression            Compression
}

func getFileRows(
	ctx context.Context, config *ProcessConfig, wg *sync.WaitGroup,
	output chan<- []byte, errs chan<- errors.E,
) {
	defer wg.Done()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var compressedReader io.Reader
	var compressedSize int64

	if config.Path != "" {
		// If we file is already available, we use it.
		compressedFile, err := os.Open(config.Path)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs <- errors.WithStack(err)
				return
			}
			// File does not exists. Continue.
		} else {
			defer compressedFile.Close()
			compressedReader = compressedFile
			compressedSize, err = compressedFile.Seek(0, io.SeekEnd)
			if err != nil {
				errs <- errors.WithStack(err)
				return
			}
			_, err = compressedFile.Seek(0, io.SeekStart)
			if err != nil {
				errs <- errors.WithStack(err)
				return
			}
		}
	}

	if compressedReader == nil {
		// File does not already exist. We download the file and optionally save it.
		req, err := retryablehttp.NewRequestWithContext(ctx, http.MethodGet, config.URL, nil)
		if err != nil {
			errs <- errors.WithStack(err)
			return
		}
		downloadReader, errE := x.NewRetryableResponse(config.Client, req)
		if errE != nil {
			errs <- errE
			return
		}
		defer downloadReader.Close()
		compressedSize = downloadReader.Size()
		if config.Path != "" {
			compressedFile, err := os.Create(config.Path)
			if err != nil {
				errs <- errors.WithStack(err)
				return
			}
			defer func() {
				info, err := os.Stat(config.Path)
				if err != nil || downloadReader.Size() != info.Size() {
					// Incomplete file. Delete.
					_ = os.Remove(config.Path)
				}
			}()
			defer compressedFile.Close()
			compressedReader = io.TeeReader(downloadReader, compressedFile)
		} else {
			compressedReader = downloadReader
		}
	}

	countingReader := &x.CountingReader{Reader: compressedReader}
	ticker := x.NewTicker(ctx, countingReader, compressedSize, progressPrintRate)
	defer ticker.Stop()
	go func() {
		for progress := range ticker.C {
			if config.Progress != nil {
				config.Progress(ctx, progress)
			}
		}
	}()

	var decompressedReader io.Reader
	switch config.Compression {
	case BZIP2, BZIP2Tar:
		decompressedReader = pbzip2.NewReader(
			ctx, countingReader,
			pbzip2.DecompressionOptions(
				pbzip2.BZConcurrency(config.DecompressionThreads),
			),
		)
	case GZIP, GZIPTar:
		gzipReader, err := gzip.NewReader(countingReader)
		if err != nil {
			errs <- errors.WithStack(err)
			return
		}
		defer gzipReader.Close()
		decompressedReader = gzipReader
	case NoCompression, Tar:
		decompressedReader = countingReader
	default:
		panic(errors.Errorf("unknown compression: %d", config.Compression))
	}

	if config.Compression == Tar || config.Compression == GZIPTar || config.Compression == BZIP2Tar {
		decompressedReader = tar.NewReader(decompressedReader)
	}

	for {
		if config.Compression == Tar || config.Compression == GZIPTar || config.Compression == BZIP2Tar {
			// Go to the first or next file in gzip/tar.
			_, err := decompressedReader.(*tar.Reader).Next()
			if err != nil {
				// When there are no more files in gzip/tar, Next returns io.EOF.
				if errors.Is(err, io.EOF) {
					// Make sure the whole file is written out to compressedFile.
					_, _ = io.Copy(io.Discard, compressedReader)
				} else {
					errs <- errors.WithStack(err)
				}
				return
			}
		}

		var iter iterator
		switch config.FileType {
		case JSONArray, NDJSON:
			iter = newJSONIterator(decompressedReader)
		case SQLDump:
			iter = newStatementIterator(decompressedReader)
		}

		if config.FileType == JSONArray {
			// Read open bracket.
			_, err := (*json.Decoder)(iter.(*jsonIterator)).Token()
			if err != nil {
				errs <- errors.WithStack(err)
				return
			}
		}

		for iter.More() {
			var row []byte
			err := iter.Next(&row)
			if err != nil {
				// Maybe More thought there was more, but there was not really more
				// after the row was fully processed.
				if errors.Is(err, io.EOF) {
					break
				}
				errs <- err
				return
			}
			select {
			case <-ctx.Done():
				errs <- errors.WithStack(ctx.Err())
				return
			case output <- row:
			}
		}

		if config.FileType == JSONArray {
			// Read closing bracket.
			_, err := (*json.Decoder)(iter.(*jsonIterator)).Token()
			if err != nil {
				errs <- errors.WithStack(err)
				return
			}
		}

		if config.Compression != Tar && config.Compression != GZIPTar && config.Compression != BZIP2Tar {
			// Only tar can have multiple files.
			break
		}
	}

	// Make sure the whole file is written out to compressedFile.
	_, _ = io.Copy(io.Discard, compressedReader)
}

// Similar to strings.ToValidUTF8, but makes sure that the number
// of bytes in the output is the same as the input. It replaces
// all invalid bytes in UTF-8 with zero byte.
func makeValid(s string) string {
	var b strings.Builder

	for i, c := range s {
		if c != utf8.RuneError {
			continue
		}

		_, wid := utf8.DecodeRuneInString(s[i:])
		if wid == 1 {
			b.Grow(len(s) + 1)
			b.WriteString(s[:i])
			s = s[i:]
			break
		}
	}

	// Fast path for unchanged input.
	if b.Cap() == 0 {
		return s
	}

	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			i++
			b.WriteByte(c)
			continue
		}
		_, wid := utf8.DecodeRuneInString(s[i:])
		if wid == 1 {
			i++
			b.WriteRune(0)
			continue
		}
		b.WriteString(s[i : i+wid])
		i += wid
	}

	return b.String()
}

func decodeJSON(ctx context.Context, itemType reflect.Type, r []byte, output chan<- interface{}, errs chan<- errors.E) {
	e := reflect.New(itemType).Interface()
	errE := x.UnmarshalWithoutUnknownFields(r, &e)
	if errE != nil {
		errs <- errors.Wrapf(errE, "cannot decode json: %s", r)
		return
	}
	select {
	case <-ctx.Done():
		errs <- errors.WithStack(ctx.Err())
		return
	case output <- e:
	}
}

func decodeRows(
	ctx context.Context, config *ProcessConfig, wg *sync.WaitGroup, decodeRowsState *syncVar,
	input <-chan []byte, output chan<- interface{}, errs chan<- errors.E,
) {
	defer wg.Done()

	itemType := reflect.TypeOf(config.Item).Elem()
	sqlParser := parser.New()
	var columns []string

	for {
		select {
		case row, ok := <-input:
			if !ok {
				return
			}

			if config.FileType == SQLDump {
				rowString := *(*string)(unsafe.Pointer(&row))
				stmt, err := sqlParser.ParseOneStmt(rowString, "", "")
				if err != nil {
					errs <- errors.Wrapf(err, "cannot parse sql: %s", row)
					return
				}
				switch s := stmt.(type) {
				case *ast.SetStmt:
					continue
				case *ast.DropTableStmt:
					continue
				case *ast.AlterTableStmt:
					continue
				case *ast.CreateTableStmt:
					cols := []string{}
					for _, col := range s.Cols {
						cols = append(cols, norm.NFC.String(col.Name.Name.O))
					}
					// Share columns with other goroutines.
					err := decodeRowsState.Store(cols)
					if err != nil {
						errs <- err
						return
					}
					columns = cols
				case *ast.InsertStmt:
					if columns == nil {
						// Wait for another goroutine to process CreateTableStmt.
						columns = decodeRowsState.Load().([]string) //nolint:errcheck
					}
					for _, r := range s.Lists {
						v := make(map[string]interface{})
						for i, column := range r {
							c, ok := column.(*test_driver.ValueExpr)
							if !ok {
								errs <- errors.Errorf("unexpected insert value %T at column %d: %s", c, i, row)
								return
							}
							z := c.GetValue()
							zz, ok := z.(string)
							if ok {
								// We have to make strings valid UTF-8 strings, otherwise they get "fixed"
								// during JSON encoding/decoding process, which can change their length,
								// which then breaks PHP decoding in DecodeImageMetadata, which is based
								// on data lengths in bytes. This is why we have to fix them and preserve
								// string length (and that of all substrings) at the same time.
								z = makeValid(zz)
							}
							v[columns[i]] = norm.NFC.String(z)
						}
						// We marshal to JSON to decode to a struct if provided.
						d, err := x.MarshalWithoutEscapeHTML(v)
						if err != nil {
							errs <- errors.WithStack(err)
							return
						}
						decodeJSON(ctx, itemType, d, output, errs)
					}
				default:
					errs <- errors.Errorf("unexpected statement %T: %s", stmt, row)
					return
				}
			} else {
				decodeJSON(ctx, itemType, row, output, errs)
			}
		case <-ctx.Done():
			errs <- errors.WithStack(ctx.Err())
			return
		}
	}
}

func processItems(
	ctx context.Context, config *ProcessConfig, wg *sync.WaitGroup,
	input <-chan interface{}, errs chan<- errors.E,
) {
	defer wg.Done()

	for {
		select {
		case i, ok := <-input:
			if !ok {
				return
			}
			err := config.Process(ctx, i)
			if err != nil {
				errs <- err
				return
			}
		case <-ctx.Done():
			errs <- errors.WithStack(ctx.Err())
			return
		}
	}
}

// Process is a low-level function which decompresses a file (supports Compression compressions),
// extacts JSONs or SQL statements from it (stored in FileType types), decodes JSONs or SQL statements, and
// calls Process callback on each decoded JSON or SQL statement. All that in parallel fashion, controlled by
// DecompressionThreads, DecodingThreads, and ItemsProcessingThreads. File is downloaded from a HTTP URL and is
// processed already during download. Downloaded file is optionally saved (to a file at Path) and followup
// calls to Process can use a saved file (if same Path is provided).
func Process(ctx context.Context, config *ProcessConfig) errors.E {
	if config.DecompressionThreads == 0 {
		config.DecompressionThreads = runtime.GOMAXPROCS(0)
	}
	if config.DecodingThreads == 0 {
		config.DecodingThreads = runtime.GOMAXPROCS(0)
	}
	if config.ItemsProcessingThreads == 0 {
		config.ItemsProcessingThreads = runtime.GOMAXPROCS(0)
	}

	// We call cancel on any error from goroutines. The expectation is that all
	// goroutines return soon afterwards.
	// TODO: Use golang.org/x/sync/errgroup instead?
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// mainWg counts groups of same goroutines.
	var mainWg sync.WaitGroup
	// mainWgChan is closed when mainWg is done.
	mainWgChan := make(chan struct{})

	errs := make(chan errors.E, 1+config.DecodingThreads+config.ItemsProcessingThreads)
	defer close(errs)

	rows := make(chan []byte, config.DecodingThreads)
	items := make(chan interface{}, config.ItemsProcessingThreads)

	var getFileRowsWg sync.WaitGroup
	mainWg.Add(1)
	getFileRowsWg.Add(1)
	go getFileRows(ctx, config, &getFileRowsWg, rows, errs)
	go func() {
		getFileRowsWg.Wait()
		mainWg.Done()
		// All goroutines using rows channel as output are done,
		// we can close the channel.
		close(rows)
	}()

	var decodeRowsWg sync.WaitGroup
	decodeRowsState := newWriteOnce()
	mainWg.Add(1)
	for w := 0; w < config.DecodingThreads; w++ {
		decodeRowsWg.Add(1)
		go decodeRows(ctx, config, &decodeRowsWg, decodeRowsState, rows, items, errs)
	}
	go func() {
		decodeRowsWg.Wait()
		mainWg.Done()
		// All goroutines using items channel as output are done,
		// we can close the channel.
		close(items)
	}()

	var processItemWg sync.WaitGroup
	mainWg.Add(1)
	for w := 0; w < config.ItemsProcessingThreads; w++ {
		processItemWg.Add(1)
		go processItems(ctx, config, &processItemWg, items, errs)
	}
	go func() {
		processItemWg.Wait()
		mainWg.Done()
	}()

	// When mainWg is done, we close mainWgChan.
	// This means that all goroutines are done.
	go func() {
		mainWg.Wait()
		close(mainWgChan)
	}()

	allErrors := []errors.E{}
WAIT:
	for {
		// We cancel the context on any error, but we also store it.
		// We also wait for all goroutines to return. The expectation
		// is that they return all when they are all successful, or
		// when there was an error and we canceled the context.
		select {
		// This function is closing errs in defer, so we do not have
		// to check if the channel is closed.
		case err := <-errs:
			allErrors = append(allErrors, err)
			cancel()
		case <-mainWgChan:
			break WAIT
		}
	}

	if len(allErrors) > 0 {
		// If there is any non-context-canceled error, return it.
		// TODO: What if there are multiple such errors?
		for _, err := range allErrors {
			if !errors.Is(err, context.Canceled) {
				return err
			}
		}

		// Otherwise return any error, i.e., the first.
		return allErrors[0]
	}

	return nil
}
