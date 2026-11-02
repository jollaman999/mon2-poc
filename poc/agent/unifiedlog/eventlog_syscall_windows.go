//go:build windows

// 이 파일은 InfluxData Telegraf 의 plugins/inputs/win_eventlog 에서 옮겼다. Copyright (c) 2015-2025 InfluxData Inc. (MIT License)

package unifiedlog

// wevtapi.dll 호출. 원본 Telegraf win_eventlog 의 zsyscall_windows.go · syscall_windows.go 를 옮겼다.
// golang.org/x/sys/windows 의 지연 DLL 로딩을 써서 CGO 가 필요 없다.

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type evtHandle uintptr

// 이벤트 로그 오류 코드
const (
	errInsufficientBuffer syscall.Errno = 122
	errNoMoreItems        syscall.Errno = 259
	errInvalidOperation   syscall.Errno = 4317
)

// EVT_SUBSCRIBE_FLAGS
type evtSubscribeFlag uint32

const (
	evtSubscribeToFutureEvents      evtSubscribeFlag = 1
	evtSubscribeStartAtOldestRecord evtSubscribeFlag = 2
	evtSubscribeStartAfterBookmark  evtSubscribeFlag = 3
)

// EVT_RENDER_FLAGS
type evtRenderFlag uint32

const (
	evtRenderEventXML evtRenderFlag = 1
	evtRenderBookmark evtRenderFlag = 2
)

// EVT_FORMAT_MESSAGE_FLAGS
type evtFormatMessageFlag uint32

const (
	evtFormatMessageEvent evtFormatMessageFlag = iota + 1
	evtFormatMessageLevel
	evtFormatMessageTask
	evtFormatMessageOpcode
	// 키워드가 여럿이면 NUL 로 끝나는 문자열이 이어져 온다.
	evtFormatMessageKeyword
)

var (
	modwevtapi = windows.NewLazySystemDLL("wevtapi.dll")

	procEvtSubscribe             = modwevtapi.NewProc("EvtSubscribe")
	procEvtRender                = modwevtapi.NewProc("EvtRender")
	procEvtClose                 = modwevtapi.NewProc("EvtClose")
	procEvtNext                  = modwevtapi.NewProc("EvtNext")
	procEvtFormatMessage         = modwevtapi.NewProc("EvtFormatMessage")
	procEvtOpenPublisherMetadata = modwevtapi.NewProc("EvtOpenPublisherMetadata")
	procEvtCreateBookmark        = modwevtapi.NewProc("EvtCreateBookmark")
	procEvtUpdateBookmark        = modwevtapi.NewProc("EvtUpdateBookmark")
)

func callErr(ok bool, e1 syscall.Errno) error {
	if ok {
		return nil
	}
	if e1 != 0 {
		return e1
	}
	return syscall.EINVAL
}

func evtSubscribe(session evtHandle, signalEvent uintptr, channelPath, query *uint16, bookmark evtHandle, context uintptr, callback syscall.Handle, flags evtSubscribeFlag) (evtHandle, error) {
	r0, _, e1 := syscall.SyscallN(procEvtSubscribe.Addr(),
		uintptr(session), signalEvent,
		uintptr(unsafe.Pointer(channelPath)), uintptr(unsafe.Pointer(query)),
		uintptr(bookmark), context, uintptr(callback), uintptr(flags))
	h := evtHandle(r0)
	return h, callErr(h != 0, e1)
}

func evtRender(fragment evtHandle, flags evtRenderFlag, bufferSize uint32, buffer *byte, bufferUsed *uint32) error {
	r1, _, e1 := syscall.SyscallN(procEvtRender.Addr(),
		0, uintptr(fragment), uintptr(flags), uintptr(bufferSize),
		uintptr(unsafe.Pointer(buffer)), uintptr(unsafe.Pointer(bufferUsed)), 0)
	return callErr(r1 != 0, e1)
}

func evtClose(object evtHandle) error {
	r1, _, e1 := syscall.SyscallN(procEvtClose.Addr(), uintptr(object))
	return callErr(r1 != 0, e1)
}

func evtNext(resultSet evtHandle, eventArraySize uint32, eventArray *evtHandle, timeout, flags uint32, numReturned *uint32) error {
	r1, _, e1 := syscall.SyscallN(procEvtNext.Addr(),
		uintptr(resultSet), uintptr(eventArraySize), uintptr(unsafe.Pointer(eventArray)),
		uintptr(timeout), uintptr(flags), uintptr(unsafe.Pointer(numReturned)))
	return callErr(r1 != 0, e1)
}

func evtFormatMessage(publisherMetadata, event evtHandle, messageID, valueCount uint32, values uintptr, flags evtFormatMessageFlag, bufferSize uint32, buffer *byte, bufferUsed *uint32) error {
	r1, _, e1 := syscall.SyscallN(procEvtFormatMessage.Addr(),
		uintptr(publisherMetadata), uintptr(event), uintptr(messageID), uintptr(valueCount),
		values, uintptr(flags), uintptr(bufferSize),
		uintptr(unsafe.Pointer(buffer)), uintptr(unsafe.Pointer(bufferUsed)))
	return callErr(r1 != 0, e1)
}

func evtOpenPublisherMetadata(session evtHandle, publisherIdentity, logFilePath *uint16, locale, flags uint32) (evtHandle, error) {
	r0, _, e1 := syscall.SyscallN(procEvtOpenPublisherMetadata.Addr(),
		uintptr(session), uintptr(unsafe.Pointer(publisherIdentity)), uintptr(unsafe.Pointer(logFilePath)),
		uintptr(locale), uintptr(flags))
	h := evtHandle(r0)
	return h, callErr(h != 0, e1)
}

func evtCreateBookmark(bookmarkXML *uint16) (evtHandle, error) {
	r0, _, e1 := syscall.SyscallN(procEvtCreateBookmark.Addr(), uintptr(unsafe.Pointer(bookmarkXML)))
	h := evtHandle(r0)
	return h, callErr(h != 0, e1)
}

func evtUpdateBookmark(bookmark, event evtHandle) error {
	r0, _, e1 := syscall.SyscallN(procEvtUpdateBookmark.Addr(), uintptr(bookmark), uintptr(event))
	return callErr(r0 != 0, e1)
}
