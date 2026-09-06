package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

type historySpool struct {
	file   *os.File
	writer *bufio.Writer
	err    error
}

func spoolLogHistory(repo string) (*historySpool, map[string]bool, error) {
	file, err := os.CreateTemp("", "git-spdx-history-*")
	if err != nil {
		return nil, nil, err
	}
	spool := &historySpool{file: file, writer: bufio.NewWriterSize(file, readerBufferSize)}
	legal := make(map[string]bool)
	err = walkChanges(repo, true, func(c change) {
		recordLegalChange(legal, c)
		if !c.merge {
			spool.write(c)
		}
	})
	if err == nil {
		err = spool.ready()
	}
	if err != nil {
		spool.close()
		return nil, nil, err
	}
	return spool, legal, nil
}

func (s *historySpool) write(c change) {
	if s.err != nil {
		return
	}
	for _, field := range [...]string{c.commit, c.date, c.subject, c.oldOID, c.newOID, c.path, c.oldMode, c.newMode} {
		if err := writeSpoolString(s.writer, field); err != nil {
			s.err = err
			return
		}
	}
}

func (s *historySpool) ready() error {
	if s.err != nil {
		return s.err
	}
	if err := s.writer.Flush(); err != nil {
		return err
	}
	_, err := s.file.Seek(0, io.SeekStart)
	return err
}

func (s *historySpool) replay(visit func(change)) error {
	reader := bufio.NewReaderSize(s.file, readerBufferSize)
	for {
		commit, err := readSpoolString(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		fields := make([]string, 7)
		for index := range fields {
			fields[index], err = readSpoolString(reader)
			if err != nil {
				return err
			}
		}
		visit(change{
			commit: commit, date: fields[0], subject: fields[1],
			oldOID: fields[2], newOID: fields[3], path: fields[4],
			oldMode: fields[5], newMode: fields[6],
		})
	}
}

func (s *historySpool) close() {
	name := s.file.Name()
	_ = s.file.Close()
	_ = os.Remove(name)
}

func writeSpoolString(writer io.Writer, value string) error {
	var encoded [binary.MaxVarintLen64]byte
	size := binary.PutUvarint(encoded[:], uint64(len(value)))
	if _, err := writer.Write(encoded[:size]); err != nil {
		return err
	}
	_, err := io.WriteString(writer, value)
	return err
}

func readSpoolString(reader *bufio.Reader) (string, error) {
	size, err := binary.ReadUvarint(reader)
	if err != nil {
		return "", err
	}
	if size > uint64(int(^uint(0)>>1)) {
		return "", fmt.Errorf("history field is too large: %d", size)
	}
	value := make([]byte, int(size))
	if _, err := io.ReadFull(reader, value); err != nil {
		return "", err
	}
	return string(value), nil
}
