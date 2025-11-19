package iscsi

/*
#cgo pkg-config: libiscsi
#include <stdlib.h>
#include "iscsi/iscsi.h"
#include "iscsi/scsi-lowlevel.h"
*/
import "C"

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/avast/retry-go/v4"
	gopointer "github.com/mattn/go-pointer"
	"golang.org/x/sys/unix"
)

var defaultLogger = atomic.Pointer[slog.Logger]{}

func init() {
	defaultLogger.Store(slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{
		Level: slog.LevelError,
	})))
}

func SetLogger(l *slog.Logger) {
	defaultLogger.Store(l)
}

func logger() *slog.Logger {
	return defaultLogger.Load()
}

type (
	iscsiContext *C.struct_iscsi_context
)

type device struct {
	Context      iscsiContext
	targetName   string
	targetPortal string
	targetLun    int
	details      ConnectionDetails
	// TODO: (willgorman) iscsiContext has a timeout (set with iscsi_set_timeout)
	// but it's not exported and there's no get function. need to track timeout in
	// device
}

type ConnectionDetails struct {
	InitiatorIQN string
	TargetURL    string
}

// Creates a new ISCSI device with the given connection details
// Note that an ISCSI device is not safe to use from multiple
// goroutines
func New(details ConnectionDetails) *device {
	return &device{
		details: details,
	}
}

func (d *device) initializeContext() error {
	if d.Context != nil {
		_ = C.iscsi_destroy_context(d.Context)
		d.Context = nil
		d.targetLun = 0
		d.targetName = ""
		d.targetPortal = ""
	}
	iqnStr := C.CString(d.details.InitiatorIQN)
	defer C.free(unsafe.Pointer(iqnStr))
	ctx := C.iscsi_create_context(iqnStr)
	targetStr := C.CString(d.details.TargetURL)
	defer C.free(unsafe.Pointer(targetStr))
	url := C.iscsi_parse_full_url(ctx, targetStr)

	if url == nil {
		return fmt.Errorf("error parsing target url: %v", C.GoString(C.iscsi_get_error(ctx)))
	}

	_ = C.iscsi_set_targetname(ctx, &url.target[0])
	defer C.iscsi_destroy_url(url)
	d.Context = ctx
	d.targetLun = int(url.lun)
	d.targetName = C.GoString(&url.target[0])
	d.targetPortal = C.GoString(&url.portal[0])
	_ = C.iscsi_set_session_type(d.Context, C.ISCSI_SESSION_NORMAL)
	_ = C.iscsi_set_header_digest(d.Context, C.ISCSI_HEADER_DIGEST_NONE_CRC32C)
	return nil
}

func (d *device) Connect() error {
	if err := d.initializeContext(); err != nil {
		return err
	}
	return retry.Do(func() error {
		portalStr := C.CString(d.targetPortal)
		defer C.free(unsafe.Pointer(portalStr))
		if retval := C.iscsi_full_connect_sync(d.Context, portalStr, C.int(d.targetLun)); retval != 0 {
			errstr := C.GoString(C.iscsi_get_error(d.Context))
			// reset the context before retrying.  it seems like some connection
			// errors leave the context in an inconsistent state that makes it
			// difficult to reuse
			d.initializeContext()
			return fmt.Errorf("iscsi_full_connect_sync: (%d) %s", retval, errstr)
		}
		return nil
	}, retry.Attempts(20), retry.MaxDelay(500*time.Millisecond))
}

func (d *device) Reconnect() error {
	if retval := C.iscsi_reconnect_sync(d.Context); retval != 0 {
		if retval != 0 {
			return fmt.Errorf("failed to reconnect with: %d", retval)
		}
	}
	return nil
}

func (d *device) Disconnect() error {
	defer C.iscsi_destroy_context(d.Context)
	retval := C.iscsi_logout_sync(d.Context)
	if retval != 0 {
		return fmt.Errorf("failed to logout with: %d", retval)
	}
	return nil
}

type Capacity struct {
	// the maximum addressable block in the device
	MaxLBA    int
	BlockSize int
}

func (d device) ReadCapacity10() (c Capacity, err error) {
	task := C.iscsi_readcapacity10_sync(d.Context, 0, 0, 0)
	defer func() {
		if task != nil {
			C.scsi_free_scsi_task(task)
		}
	}()
	if task == nil || task.status != C.SCSI_STATUS_GOOD {
		errstr := C.iscsi_get_error(d.Context)
		return c, fmt.Errorf("iscsi_readcapacity10_sync: %s", C.GoString(errstr))
	}
	readcapacity, err := getReadCapacity10(*task)
	if err != nil {
		return c, err
	}
	c.BlockSize = int(readcapacity.block_size)
	c.MaxLBA = int(readcapacity.lba)
	logger().Debug("ReadCapacity10", slog.Any("capacity", c))
	return c, nil
}

func (d device) ReadCapacity16() (c Capacity, err error) {
	task := C.iscsi_readcapacity16_sync(d.Context, 0)
	defer func() {
		if task != nil {
			C.scsi_free_scsi_task(task)
		}
	}()
	if task == nil || task.status != C.SCSI_STATUS_GOOD {
		errstr := C.iscsi_get_error(d.Context)
		return c, fmt.Errorf("iscsi_readcapacity16_sync: %s", C.GoString(errstr))
	}
	readcapacity, err := getReadCapacity16(*task)
	if err != nil {
		return c, err
	}
	c.BlockSize = int(readcapacity.block_length)
	c.MaxLBA = int(readcapacity.returned_lba)
	logger().Debug("ReadCapacity16", slog.Any("capacity", c))
	return c, nil
}

type Write16 struct {
	LBA       int
	Data      []byte
	BlockSize int
}

func (d *device) Write16(data Write16) error {
	logger().Debug("Write16", slog.Any("request", data))
	state := &syncCallbackState{}
	pdata := gopointer.Save(state)
	defer gopointer.Unref(pdata)
	carr := []C.uchar(string(data.Data))
	// TODO: (willgorman) figure out why larger blocksizes cause SCSI_SENSE_ASCQ_INVALID_FIELD_IN_INFORMATION_UNIT
	if C.iscsi_write16_task(
		d.Context, 0, C.uint64_t(data.LBA), &carr[0], C.uint(len(carr)),
		C.int(data.BlockSize), 0, 0, 0, 0, 0, syncCB, pdata,
	) == nil {
		return errors.New("unable to start iscsi_write16_task")
	}

	if err := d.eventLoop(state); err != nil {
		return fmt.Errorf("error while waiting for task completion: %w", err)
	}
	task := state.scsiTask
	if task != nil {
		defer C.scsi_free_scsi_task(task)
	}
	if task == nil || task.status != C.SCSI_STATUS_GOOD {
		// TODO: (willgorman) robust error checking of condition, sense key, etc
		// from libiscsi
		// ok = task->status == SCSI_STATUS_GOOD ||
		// (task->status == SCSI_STATUS_CHECK_CONDITION &&
		//  task->sense.key == SCSI_SENSE_ILLEGAL_REQUEST &&
		//  task->sense.ascq == SCSI_SENSE_ASCQ_INVALID_FIELD_IN_INFORMATION_UNIT);
		errstr := C.iscsi_get_error(d.Context)
		return fmt.Errorf("iscsi_write16_sync: %s", C.GoString(errstr))
	}
	logger().Debug("Write16 done", slog.Any("request", data))
	return nil
}

type Read16 struct {
	LBA       int
	Blocks    int
	BlockSize int
}

func (d *device) Read16(data Read16) ([]byte, error) {
	state := &syncCallbackState{}
	pdata := gopointer.Save(state)
	defer gopointer.Unref(pdata)

	if C.iscsi_read16_task(
		d.Context, 0, C.uint64_t(data.LBA),
		C.uint(data.BlockSize*data.Blocks), C.int(data.BlockSize),
		0, 0, 0, 0, 0, syncCB, pdata,
	) == nil {
		return nil, errors.New("unable to start iscsi_read16_task")
	}
	if err := d.eventLoop(state); err != nil {
		return nil, fmt.Errorf("error while waiting for task completion: %w", err)
	}

	task := state.scsiTask
	if task != nil {
		defer C.scsi_free_scsi_task(task)
	}

	if task == nil || task.status != C.SCSI_STATUS_GOOD {
		return nil, fmt.Errorf("iscsi_read16_sync: %s", C.GoString(C.iscsi_get_error(d.Context)))
	}
	logger().Debug("Read16 done", slog.Any("length", task.datain.size))
	return C.GoBytes(unsafe.Pointer(task.datain.data), task.datain.size), nil
}

func (d *device) Read16Async(data Read16, tasks chan TaskResult) error {
	cdata := callbackData{
		tasks: tasks,
		// add the read request so the callback can tell what lba the read
		// started at.  i suspect this is probably also in the scsi_task
		// but this is simple enough for now
		context: data,
	}
	pdata := gopointer.Save(cdata)
	// can't call unref until the callback is done
	task := C.iscsi_read16_task(d.Context, 0, C.uint64_t(data.LBA),
		C.uint(data.BlockSize*data.Blocks), C.int(data.BlockSize), 0, 0, 0, 0, 0, channelCB, pdata)
	if task == nil {
		return errors.New("unable to start iscsi_read16_task")
	}

	return nil
}

func (d *device) eventLoop(state *syncCallbackState) error {
	// TODO: (willgorman) handle a timeout (from iscsi_set_timeout)
	// this gets set by iscsiSyncCB
	for !state.finished {
		events := d.WhichEvents()
		fd := unix.PollFd{
			Fd:      int32(d.GetFD()),
			Events:  int16(events),
			Revents: 0,
		}

		fds := []unix.PollFd{fd}
		_, err := unix.Poll(fds, 1000)
		// it's fine if the syscall got interrupted by a signal, we can just
		// resume polling
		if err != nil && err != syscall.EINTR {
			return fmt.Errorf("poll failed: %w", err)
		}
		if d.HandleEvents(fds[0].Revents) < 0 {
			return fmt.Errorf("failed to handle events: %s",
				C.GoString(C.iscsi_get_error(d.Context)))
		}
	}
	return nil
}

// TODO: Implement WriteSame

// this is not safe to run in a goroutine other than the one
// where all other operations on the iscsi connection are
// being performed
// TODO: i'm not sure this function even makes sense because
// it can't run concurrently with iscsi operations
func (d *device) ProcessAsync(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			events := d.WhichEvents()
			if events == 0 {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			fd := unix.PollFd{
				Fd:      int32(d.GetFD()),
				Events:  int16(events),
				Revents: 0,
			}

			fds := []unix.PollFd{fd}
			_, err := unix.Poll(fds, 1000)
			if err != nil {
				if err.Error() != "interrupted system call" {
					return fmt.Errorf("Poll error: %w", err)
				}
			}
			// I think we have to call this with fds[0], not fd.
			// fds[0] is what actually gets updated, fd is just a copy
			if d.HandleEvents(fds[0].Revents) < 0 {
				return errors.New("failed to handle events")
			}
		}
	}
}

func (d *device) ProcessAsyncN(n int) error {
	for i := 0; i < n; i++ {
		events := d.WhichEvents()
		if events == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		fd := unix.PollFd{
			Fd:      int32(d.GetFD()),
			Events:  int16(events),
			Revents: 0,
		}

		fds := []unix.PollFd{fd}
		_, err := unix.Poll(fds, 1000)
		if err != nil {
			if err.Error() != "interrupted system call" {
				return fmt.Errorf("Poll error: %w", err)
			}
		}
		// I think we have to call this with fds[0], not fd.
		// fds[0] is what actually gets updated, fd is just a copy
		if d.HandleEvents(fds[0].Revents) < 0 {
			return errors.New("failed to handle events")
		}
	}
	return nil
}

func (d *device) GetFD() int {
	return int(C.iscsi_get_fd(d.Context))
}

func (d *device) WhichEvents() int {
	return int(C.iscsi_which_events(d.Context))
}

func (d *device) HandleEvents(n int16) int {
	return int(C.iscsi_service(d.Context, C.int(n)))
}

func (d *device) GetQueueLength() int {
	return int(C.iscsi_queue_length(d.Context))
}

func (d *device) GetOutQueueLength() int {
	return int(C.iscsi_out_queue_length(d.Context))
}

func getReadCapacity10(task C.struct_scsi_task) (C.struct_scsi_readcapacity10, error) {
	cap := C.struct_scsi_readcapacity10{}

	if task.cdb[0] != C.SCSI_OPCODE_READCAPACITY10 {
		return cap, errors.New("unexpected opcode")
	}
	if task.datain.size != 8 {
		return cap, errors.New("unexpected size")
	}

	dataread := unsafe.Slice(task.datain.data, task.datain.size)
	databytes := []byte(string(dataread))
	cap.lba = C.uint(binary.BigEndian.Uint32(databytes[:4]))
	cap.block_size = C.uint(binary.BigEndian.Uint32(databytes[4:]))
	return cap, nil
}

func getReadCapacity16(task C.struct_scsi_task) (C.struct_scsi_readcapacity16, error) {
	cap := C.struct_scsi_readcapacity16{}
	if task.cdb[0] != C.SCSI_OPCODE_SERVICE_ACTION_IN {
		return cap, errors.New("unexpected opcode")
	}
	if task.cdb[1] != C.SCSI_READCAPACITY16 {
		return cap, errors.New("unexpected subaction")
	}

	scsiTask := scsiTask{task: task}.init()
	cap.returned_lba = (C.ulong(scsiTask.getUint32(0))<<32 | C.ulong(scsiTask.getUint32(4)))
	cap.block_length = scsiTask.getUint32(8)
	cap.p_type = (scsiTask.getUint8(12) >> 1) & 0x07
	cap.prot_en = scsiTask.getUint8(12) & 0x01
	cap.p_i_exp = (scsiTask.getUint8(13) >> 4) & 0x0f
	cap.lbppbe = scsiTask.getUint8(13) & 0x0f
	cap.lbpme = (scsiTask.getUint8(14) & 0x80)
	if cap.lbpme != 0 {
		cap.lbpme = 1
	}
	cap.lbprz = (scsiTask.getUint8(14) & 0x40)
	if cap.lbprz != 0 {
		cap.lbprz = 1
	}
	cap.lalba = scsiTask.getUint16(14) & 0x3fff
	return cap, nil
}

type scsiTask struct {
	task   C.struct_scsi_task
	dataIn []byte
}

func (s scsiTask) init() scsiTask {
	dataread := unsafe.Slice(s.task.datain.data, s.task.datain.size)
	s.dataIn = []byte(string(dataread))
	return s
}

func (s scsiTask) getUint32(offset int) C.uint {
	if len(s.dataIn) == 0 {
		panic("uninitialized scsiTask")
	}
	if offset <= int(s.task.datain.size-4) {
		return C.uint(binary.BigEndian.Uint32(s.dataIn[offset : offset+4]))
	}
	return 0
}

func (s scsiTask) getUint16(offset int) C.uint16_t {
	if len(s.dataIn) == 0 {
		panic("uninitialized scsiTask")
	}
	if offset <= int(s.task.datain.size-2) {
		return C.uint16_t(binary.BigEndian.Uint16(s.dataIn[offset : offset+2]))
	}
	return 0
}

func (s scsiTask) getUint8(offset int) C.uint8_t {
	if len(s.dataIn) == 0 {
		panic("uninitialized scsiTask")
	}
	if offset <= int(s.task.datain.size-1) {
		return C.uint8_t(s.dataIn[offset])
	}
	return 0
}

// Don't want to expose C structs to callers and can't
// embed C structs in a Go struct so we need getters
type Task struct {
	Status int
	DataIn []byte
}

type callbackData struct {
	tasks   chan TaskResult
	context any
}

type syncCallbackState struct {
	finished bool
	status   int
	scsiTask *C.struct_scsi_task
}

type TaskResult struct {
	Task    Task
	Err     error
	Context any
}

//export iscsiChannelCB
func iscsiChannelCB(iscsiCtx iscsiContext, status int, command_data, private_data unsafe.Pointer) {
	defer gopointer.Unref(private_data)
	data := gopointer.Restore(private_data).(callbackData)

	if status != C.SCSI_STATUS_GOOD {
		data.tasks <- TaskResult{
			Err: errors.New(C.GoString(C.iscsi_get_error(iscsiCtx))),
		}
		return
	}
	// get command data onto the channel
	task := (*C.struct_scsi_task)(command_data)
	defer C.free(command_data)

	data.tasks <- TaskResult{
		Task: Task{
			Status: int(task.status),
			DataIn: C.GoBytes(unsafe.Pointer(task.datain.data), task.datain.size),
		},
		Context: data.context,
	}
}

//export iscsiSyncCB
func iscsiSyncCB(_ iscsiContext, status int, command_data, private_data unsafe.Pointer) {
	task := (*C.struct_scsi_task)(command_data)
	state := gopointer.Restore(private_data).(*syncCallbackState)
	task.status = C.int(status)
	state.status = status
	state.finished = true
	state.scsiTask = task
}

func printReadCapacity16(t *C.struct_scsi_readcapacity16) {
	log.Printf("blocklength: %d\n", t.block_length)
	log.Printf("lalba: %d\n", t.lalba)
	log.Printf("lbpme: %d\n", t.lbpme)
	log.Printf("lbppbe: %d\n", t.lbppbe)
	log.Printf("lbprz: %d\n", t.lbprz)
	log.Printf("piexp: %d\n", t.p_i_exp)
	log.Printf("ptype: %d\n", t.p_type)
	log.Printf("proten: %d\n", t.prot_en)
	log.Printf("returnedlba: %d\n", t.returned_lba)
}
