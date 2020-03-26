package masker

import (
	"bytes"
	"io"
	"sync"
)

// stream is a buffered io.Writer that masks all secrets written on it using a multipleMatcher.
type stream struct {
	dest          io.Writer
	buf           IndexedBuffer
	registerFrame func(*stream, int)

	matcher     *multipleMatcher
	matches     Matches
	matchesLock sync.Mutex
}

// Write implements the io.Writer interface for the stream.
// The written frame is stored in the buffer and it is registered in the Masker to make sure it is flushed from
// the buffer after the buffer delay has passed.
// The bytes are also passed to the secret matcher to check for any matches with secrets.
func (s *stream) Write(p []byte) (int, error) {
	n, err := s.buf.Write(p)
	if n > 0 {
		s.registerFrame(s, n)
	}

	for index, length := range s.matcher.Write(p[:n]) {
		s.addMatch(index, length)
	}

	return n, err
}

// addMatch adds the match of a secret at the given index and with the given length to the map of matches if the
// associated bytes have not yet been written to the destination.
func (s *stream) addMatch(index int64, length int) {
	s.matchesLock.Lock()
	defer s.matchesLock.Unlock()

	if index >= s.buf.CurrentIndex() {
		s.matches = s.matches.Add(index, length)
	}
}

// flush n bytes from the buffer and mask any secrets that have been matched.
func (s *stream) flush(n int) error {
	startIndex := s.buf.CurrentIndex()
	endIndex := startIndex + int64(n)

	// Increment the frameIndex before processing matches to avoid adding new matches in the processed frame.
	for i := startIndex; i < endIndex; i++ {
		s.matchesLock.Lock()
		length, exists := s.matches[i]
		s.matchesLock.Unlock()

		if exists {
			// Get any unprocessed bytes before this match to the destination.
			beforeMatch := s.buf.UpToIndex(i)

			_, err := s.dest.Write(beforeMatch)
			if err != nil {
				return err
			}

			// Only write the redaction text if there were bytes between this match and the previous match
			// or this is the first flush for the buffer.
			if len(beforeMatch) > 0 || s.buf.CurrentIndex() == 0 {
				_, err = s.dest.Write([]byte("<redacted by SecretHub>"))
				if err != nil {
					return err
				}
			}

			// Drop all bytes until the end of the mask.
			_ = s.buf.UpToIndex(i + int64(length))

			delete(s.matches, i)
		}
	}

	// Write all bytes after the last match.
	_, err := s.dest.Write(s.buf.UpToIndex(endIndex))
	if err != nil {
		return err
	}

	return nil
}

// IndexedBuffer is a goroutine safe buffer that assigns every byte that is written to it with an incrementing index.
type IndexedBuffer struct {
	buffer       bytes.Buffer
	mutex        sync.Mutex
	currentIndex int64
}

func (b *IndexedBuffer) CurrentIndex() int64 {
	return b.currentIndex
}

func (b *IndexedBuffer) Write(p []byte) (n int, err error) {
	b.mutex.Lock()
	defer b.mutex.Unlock()
	return b.buffer.Write(p)
}

// UpToIndex pops and returns all bytes in the buffer up to the given index.
// If all bytes up to this given index have already been returned previously, an empty slice is returned.
func (b *IndexedBuffer) UpToIndex(index int64) []byte {
	b.mutex.Lock()
	defer b.mutex.Unlock()

	if index < b.currentIndex {
		return []byte{}
	}
	n := int(index - b.currentIndex)
	b.currentIndex = index
	return b.buffer.Next(n)
}