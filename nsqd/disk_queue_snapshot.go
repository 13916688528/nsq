package nsqd

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"github.com/absolute8511/nsq/internal/levellogger"
	"io"
	"os"
	"sync"
)

// note: the message count info is not kept in snapshot
type DiskQueueSnapshot struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	queueStart diskQueueEndInfo
	readPos    diskQueueEndInfo
	endPos     diskQueueEndInfo

	sync.RWMutex

	readFrom string
	dataPath string
	exitFlag int32

	readFile *os.File
	reader   *bufio.Reader
}

// newDiskQueue instantiates a new instance of DiskQueueSnapshot, retrieving metadata
// from the filesystem and starting the read ahead goroutine
func NewDiskQueueSnapshot(readFrom string, dataPath string, endInfo BackendQueueEnd) *DiskQueueSnapshot {
	d := DiskQueueSnapshot{
		readFrom: readFrom,
		dataPath: dataPath,
	}

	d.UpdateQueueEnd(endInfo)

	return &d
}

func (d *DiskQueueSnapshot) getCurrentFileEnd(offset diskQueueOffset) (int64, error) {
	curFileName := d.fileName(offset.FileNum)
	f, err := os.Stat(curFileName)
	if err != nil {
		return 0, err
	}
	return f.Size(), nil
}

func (d *DiskQueueSnapshot) SetQueueStart(start BackendQueueEnd) {
	startPos, ok := start.(*diskQueueEndInfo)
	if !ok || startPos == nil {
		return
	}
	d.Lock()
	defer d.Unlock()

	if d.exitFlag == 1 {
		return
	}
	nsqLog.Infof("topic : %v change start from %v to %v", d.readFrom, d.queueStart, startPos)
	d.queueStart = *startPos
	if d.queueStart.EndOffset.GreatThan(&d.readPos.EndOffset) {
		d.readPos = d.queueStart
	}
}

func (d *DiskQueueSnapshot) GetQueueReadStart() BackendQueueEnd {
	d.Lock()
	defer d.Unlock()
	s := d.queueStart
	return &s
}

// Put writes a []byte to the queue
func (d *DiskQueueSnapshot) UpdateQueueEnd(e BackendQueueEnd) {
	endPos, ok := e.(*diskQueueEndInfo)
	if !ok || endPos == nil {
		return
	}
	d.Lock()
	defer d.Unlock()

	if d.exitFlag == 1 {
		return
	}
	if d.readPos.EndOffset.GreatThan(&endPos.EndOffset) {
		d.readPos = *endPos
	}
	d.endPos = *endPos
}

// Close cleans up the queue and persists metadata
func (d *DiskQueueSnapshot) Close() error {
	return d.exit()
}

func (d *DiskQueueSnapshot) exit() error {
	d.Lock()

	d.exitFlag = 1
	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}
	d.Unlock()
	return nil
}

func (d *DiskQueueSnapshot) GetCurrentReadQueueOffset() BackendQueueOffset {
	d.Lock()
	cur := d.readPos
	var tmp diskQueueOffsetInfo
	tmp.EndOffset = cur.EndOffset
	tmp.virtualEnd = cur.virtualEnd
	d.Unlock()
	return &tmp
}

func (d *DiskQueueSnapshot) stepOffset(allowBackward bool, cur diskQueueEndInfo, step int64, maxStep diskQueueEndInfo) (diskQueueOffset, error) {
	newOffset := cur
	if cur.EndOffset.FileNum > maxStep.EndOffset.FileNum {
		return newOffset.EndOffset, fmt.Errorf("offset invalid: %v , %v", cur, maxStep)
	}
	if step == 0 {
		return newOffset.EndOffset, nil
	}
	if !allowBackward && step < 0 {
		return newOffset.EndOffset, fmt.Errorf("can not step backward")
	}
	return stepOffset(d.dataPath, d.readFrom, cur, BackendOffset(step), maxStep)
}

func (d *DiskQueueSnapshot) SkipToNext() error {
	d.Lock()
	defer d.Unlock()
	if d.readPos.EndOffset.FileNum >= d.endPos.EndOffset.FileNum {
		return ErrReadEndOfQueue
	}

	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}
	_, _, endPos, err := getQueueFileOffsetMeta(d.fileName(d.readPos.EndOffset.FileNum))
	if err != nil {
		return err
	}
	d.readPos.EndOffset.FileNum++
	d.readPos.EndOffset.Pos = 0
	d.readPos.virtualEnd = BackendOffset(endPos)
	return nil
}

// this can allow backward seek
func (d *DiskQueueSnapshot) ResetSeekTo(voffset BackendOffset) error {
	return d.seekTo(voffset, true)
}

func (d *DiskQueueSnapshot) SeekTo(voffset BackendOffset) error {
	return d.seekTo(voffset, false)
}

func (d *DiskQueueSnapshot) seekTo(voffset BackendOffset, allowBackward bool) error {
	d.Lock()
	defer d.Unlock()
	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}
	var err error
	newPos := d.endPos.EndOffset
	if voffset > d.endPos.virtualEnd {
		nsqLog.LogErrorf("internal skip error : skipping overflow to : %v, %v", voffset, d.endPos)
		return ErrMoveOffsetInvalid
	} else if voffset == d.endPos.virtualEnd {
		newPos = d.endPos.EndOffset
	} else {
		if voffset < d.queueStart.Offset() {
			nsqLog.LogWarningf("seek error : seek queue position cleaned : %v, %v", voffset, d.queueStart)
			return ErrReadQueueAlreadyCleaned
		}

		newPos, err = d.stepOffset(allowBackward, d.readPos, int64(voffset-d.readPos.virtualEnd), d.endPos)
		if err != nil {
			nsqLog.LogErrorf("internal skip error : %v, step from %v to : %v, current start: %v", err, d.readPos, voffset, d.queueStart)
			return err
		}
	}

	nsqLog.LogDebugf("===snapshot read seek from %v, %v to: %v, %v", d.readPos,
		d.readPos, newPos, voffset)
	d.readPos.EndOffset = newPos
	d.readPos.virtualEnd = voffset
	return nil
}

func (d *DiskQueueSnapshot) SeekToEnd() error {
	d.Lock()
	if d.readFile != nil {
		d.readFile.Close()
		d.readFile = nil
	}

	d.readPos = d.endPos
	d.Unlock()
	return nil
}

func (d *DiskQueueSnapshot) ReadRaw(size int32) ([]byte, error) {
	d.Lock()
	defer d.Unlock()

	result := make([]byte, size)
	readOffset := int32(0)
	var stat os.FileInfo
	var err error

	for readOffset < size {

	CheckFileOpen:
		if d.readFile == nil {
			curFileName := d.fileName(d.readPos.EndOffset.FileNum)
			d.readFile, err = os.OpenFile(curFileName, os.O_RDONLY, 0600)
			if err != nil {
				return result, err
			}
			nsqLog.LogDebugf("DISKQUEUE snapshot(%s): readRaw() opened %s", d.readFrom, curFileName)
			if d.readPos.EndOffset.Pos > 0 {
				_, err = d.readFile.Seek(d.readPos.EndOffset.Pos, 0)
				if err != nil {
					d.readFile.Close()
					d.readFile = nil
					return result, err
				}
			}
			d.reader = bufio.NewReader(d.readFile)
		}
		stat, err = d.readFile.Stat()
		if err != nil {
			return result, err
		}
		if d.readPos.EndOffset.FileNum < d.endPos.EndOffset.FileNum {
			if d.readPos.EndOffset.Pos >= stat.Size() {
				d.readPos.EndOffset.FileNum++
				d.readPos.EndOffset.Pos = 0
				nsqLog.Logf("DISKQUEUE snapshot(%s): readRaw() read end, try next: %v",
					d.readFrom, d.readPos)
				d.readFile.Close()
				d.readFile = nil
				goto CheckFileOpen
			}
		}

		fileLeft := stat.Size() - d.readPos.EndOffset.Pos
		if d.readPos.EndOffset.FileNum == d.endPos.EndOffset.FileNum && fileLeft == 0 {
			return result[:readOffset], io.EOF
		}
		currentRead := int64(size - readOffset)
		if fileLeft < currentRead {
			currentRead = fileLeft
		}
		_, err = io.ReadFull(d.reader, result[readOffset:int64(readOffset)+currentRead])
		if err != nil {
			d.readFile.Close()
			d.readFile = nil
			return result, err
		}

		oldPos := d.readPos
		d.readPos.EndOffset.Pos = d.readPos.EndOffset.Pos + currentRead
		readOffset += int32(currentRead)
		d.readPos.virtualEnd += BackendOffset(currentRead)
		if nsqLog.Level() >= levellogger.LOG_DETAIL {
			nsqLog.LogDebugf("===snapshot read move forward: %v to  %v", oldPos,
				d.readPos)
		}

		isEnd := false
		if d.readPos.EndOffset.FileNum < d.endPos.EndOffset.FileNum {
			isEnd = d.readPos.EndOffset.Pos >= stat.Size()
		}
		if isEnd {
			if d.readFile != nil {
				d.readFile.Close()
				d.readFile = nil
			}
			d.readPos.EndOffset.FileNum++
			d.readPos.EndOffset.Pos = 0
		}
	}
	return result, nil
}

// readOne performs a low level filesystem read for a single []byte
// while advancing read positions and rolling files, if necessary
func (d *DiskQueueSnapshot) ReadOne() ReadResult {
	d.Lock()
	defer d.Unlock()

	var result ReadResult
	var msgSize int32
	var stat os.FileInfo
	result.Offset = BackendOffset(0)
	if d.readPos == d.endPos {
		result.Err = io.EOF
		return result
	}

CheckFileOpen:

	result.Offset = d.readPos.virtualEnd
	if d.readFile == nil {
		curFileName := d.fileName(d.readPos.EndOffset.FileNum)
		d.readFile, result.Err = os.OpenFile(curFileName, os.O_RDONLY, 0600)
		if result.Err != nil {
			return result
		}

		nsqLog.LogDebugf("DISKQUEUE(%s): readOne() opened %s", d.readFrom, curFileName)

		if d.readPos.EndOffset.Pos > 0 {
			_, result.Err = d.readFile.Seek(d.readPos.EndOffset.Pos, 0)
			if result.Err != nil {
				d.readFile.Close()
				d.readFile = nil
				return result
			}
		}

		d.reader = bufio.NewReader(d.readFile)
	}
	if d.readPos.EndOffset.FileNum < d.endPos.EndOffset.FileNum {
		stat, result.Err = d.readFile.Stat()
		if result.Err != nil {
			return result
		}
		if d.readPos.EndOffset.Pos >= stat.Size() {
			d.readPos.EndOffset.FileNum++
			d.readPos.EndOffset.Pos = 0
			nsqLog.Logf("DISKQUEUE(%s): readOne() read end, try next: %v",
				d.readFrom, d.readPos.EndOffset.FileNum)
			d.readFile.Close()
			d.readFile = nil
			goto CheckFileOpen
		}
	}

	result.Err = binary.Read(d.reader, binary.BigEndian, &msgSize)
	if result.Err != nil {
		d.readFile.Close()
		d.readFile = nil
		return result
	}

	if msgSize <= 0 || msgSize > MAX_POSSIBLE_MSG_SIZE {
		// this file is corrupt and we have no reasonable guarantee on
		// where a new message should begin
		d.readFile.Close()
		d.readFile = nil
		result.Err = fmt.Errorf("invalid message read size (%d)", msgSize)
		return result
	}

	result.Data = make([]byte, msgSize)
	_, result.Err = io.ReadFull(d.reader, result.Data)
	if result.Err != nil {
		d.readFile.Close()
		d.readFile = nil
		return result
	}

	result.Offset = d.readPos.virtualEnd

	totalBytes := int64(4 + msgSize)
	result.MovedSize = BackendOffset(totalBytes)

	oldPos := d.readPos
	d.readPos.EndOffset.Pos = d.readPos.EndOffset.Pos + totalBytes
	d.readPos.virtualEnd += BackendOffset(totalBytes)
	nsqLog.LogDebugf("=== read move forward: %v to %v", oldPos,
		d.readPos)

	isEnd := false
	if d.readPos.EndOffset.FileNum < d.endPos.EndOffset.FileNum {
		stat, result.Err = d.readFile.Stat()
		if result.Err == nil {
			isEnd = d.readPos.EndOffset.Pos >= stat.Size()
		} else {
			return result
		}
	}
	if isEnd {
		if d.readFile != nil {
			d.readFile.Close()
			d.readFile = nil
		}

		d.readPos.EndOffset.FileNum++
		d.readPos.EndOffset.Pos = 0
	}
	return result
}

func (d *DiskQueueSnapshot) fileName(fileNum int64) string {
	return GetQueueFileName(d.dataPath, d.readFrom, fileNum)
}
