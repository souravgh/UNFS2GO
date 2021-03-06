// Due to a limitation in CGO, exports must be in a separate file from func main
package main

//#include "unfs3/daemon.h"
import "C"
import (
	"encoding/binary"
	"fmt"
	"os"
	pathpkg "path"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const PTRSIZE = 32 << uintptr(^uintptr(0)>>63) //bit size of pointers (32 or 64)
const PTRBYTES = PTRSIZE / 8                   //bytes of the above

var fddb fdCache //translator for file descriptors

//export go_init
func go_init() C.int {
	fddb = fdCache{FDlistLock: new(sync.RWMutex),
		PathMapA:  make(map[string]int),
		PathMapB:  make(map[int]string),
		FDcounter: 100}
	return 1
}

//export go_accept_mount
func go_accept_mount(addr C.uint32, path *C.char) C.int {
	a := uint32(addr)
	hostaddress := fmt.Sprintf("%d.%d.%d.%d", byte(a), byte(a>>8), byte(a>>16), byte(a>>24))
	gpath := pathpkg.Clean("/" + C.GoString(path))
	retVal, _ := errTranslator(nil)
	if strings.EqualFold(hostaddress, "127.0.0.1") { //TODO: Make this configurable
		fmt.Println("Host allowed to connect:", hostaddress, "path:", gpath)
	} else {
		fmt.Println("Host not allowed to connect:", hostaddress, "path:", gpath)
		retVal, _ = errTranslator(os.ErrPermission)
	}
	return retVal
}

//Readdir results are in the form of two char arrays: One for the entries, and one for the actual file names
//the names array is just the file names, places at every maxpathlen bytes
//the entries array is made of entry structs, which are 24-bytes or 32-bytes long:
//[8-byte inode][4-byte or 8-byte pointer to filename]
//[8-byte cookie (entry index in directory list)][4-byte or 8-byte pointer to next entry]

//export go_readdir_full
func go_readdir_full(dirpath *C.char, cookie C.uint64, count C.uint32, names unsafe.Pointer,
	entries unsafe.Pointer, maxpathlen C.int, maxentries C.int) C.int {
	mp := int(maxpathlen)
	me := int(maxentries)

	entrySize := 24
	if PTRSIZE == 64 {
		entrySize = 32
	}

	maxByteCount := int(count)
	startCookie := int(cookie)

	nslice := &reflect.SliceHeader{Data: uintptr(names), Len: mp * me, Cap: mp * me}
	newNames := *(*[]byte)(unsafe.Pointer(nslice))

	eslice := &reflect.SliceHeader{Data: uintptr(entries), Len: entrySize * me, Cap: entrySize * me}
	newEntries := *(*[]byte)(unsafe.Pointer(eslice))

	//null out everything
	for i := range newNames {
		newNames[i] = 0
	}
	//null out everything
	for i := range newEntries {
		newEntries[i] = 0
	}

	dirp := pathpkg.Clean("/" + C.GoString(dirpath))

	arr, err := ns.ReadDirectory(dirp)
	if err != nil {
	retVal, known := errTranslator(err)
	if !known {
			fmt.Println("Error on go_readdir_full of", dirp, ":", err)
		}
		return retVal
	}

	if startCookie > len(arr) { //if asked for a higher index than exists in dir
		fmt.Println("Readdir got a bad cookie (", startCookie, ") for", dirp)
		return C.NFS3ERR_BAD_COOKIE
	}

	nbIndex := 0 //current index in names buffer
	ebIndex := 0 //current index in entry buffer

	namepointer := uint64(uintptr(names))
	entriespointer := uint64(uintptr(entries))

	for i := startCookie; i < len(arr); i++ {
		fi := arr[i]
		//name string + null terminator + 2 64-bit numbers + 2 pointers
		maxByteCount -= len(fi.Name()) + 1 + 16 + 2*PTRBYTES

		if maxByteCount < 0 || (i-startCookie) >= me {
			return -1 //signify that we didn't reach eof
		}

		if i != startCookie { //only if this isn't the first entry
			//Put a pointer to this entry as previous entry's Next
			if PTRSIZE == 32 {
				binary.LittleEndian.PutUint32(newEntries[ebIndex-PTRBYTES:], uint32(entriespointer)+uint32(ebIndex))
			} else {
				binary.LittleEndian.PutUint64(newEntries[ebIndex-PTRBYTES:], entriespointer+uint64(ebIndex))
			}
		}

		fp := pathpkg.Clean(dirp + "/" + fi.Name())

		//Put FileID
		fd := fddb.GetFD(fp)
		binary.LittleEndian.PutUint64(newEntries[ebIndex:], uint64(fd))
		ebIndex += 8

		//Put Pointer to Name
		if PTRSIZE == 32 {
			binary.LittleEndian.PutUint32(newEntries[ebIndex:], uint32(namepointer)+uint32(nbIndex))
		} else {
			binary.LittleEndian.PutUint64(newEntries[ebIndex:], namepointer+uint64(nbIndex))
		}
		ebIndex += PTRBYTES

		//Actually write Name to namebuf
		bytCount := copy(newNames[nbIndex:], []byte(fi.Name()))
		newNames[nbIndex+bytCount] = byte(0) //null terminate
		nbIndex += mp

		//Put Cookie
		binary.LittleEndian.PutUint64(newEntries[ebIndex:], uint64(i+1))
		ebIndex += 8

		//Null out this pointer to "next" in case we're the last entry
		if PTRSIZE == 32 {
		binary.LittleEndian.PutUint32(newEntries[ebIndex:], uint32(0))
		} else {
			binary.LittleEndian.PutUint64(newEntries[ebIndex:], uint64(0))
		}

		ebIndex += PTRBYTES
	}

	return C.NFS3_OK
}

//export go_fgetpath
func go_fgetpath(fd C.int) *C.char {
	gofd := int(fd)
	path, err := fddb.GetPath(gofd)
	if err != nil {
		fmt.Println("Error on go_fgetpath (fd =", gofd, " of ", fddb.FDcounter, ");", err)
		return nil
	} else {
		//fmt.Println("go_fgetpath: Returning '", path, "' for fd:", gofd)
		return C.CString(path)
	}
}

//bool is true if error recognized, otherwise false
func errTranslator(err error) (C.int, bool) {
	switch err {
	case nil:
		return C.NFS3_OK, true
	case os.ErrPermission:
		return C.NFS3ERR_ACCES, true
	case os.ErrNotExist:
		return C.NFS3ERR_NOENT, true
	case os.ErrInvalid:
		return C.NFS3ERR_INVAL, true
	case os.ErrExist:
		return C.NFS3ERR_EXIST, true
	default:
		switch {
		case strings.Contains(err.Error(), "not empty"):
			return C.NFS3ERR_NOTEMPTY, true
		default:
			return C.NFS3ERR_IO, false
		}
	}
}

//export go_lstat
func go_lstat(path *C.char, buf *C.go_statstruct) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	fi, err := ns.Stat(pp)
	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error on lstat of", pp, "):", err)
	}
	if err == nil {
		statTranslator(fi, fddb.GetFD(pp), buf)
	}
	return retVal
}

func statTranslator(fi os.FileInfo, fd_ino int, buf *C.go_statstruct) {
	buf.st_dev = C.uint32(1)
	buf.st_ino = C.uint64(fd_ino)
	buf.st_size = C.uint64(fi.Size())
	buf.st_atime = C.time_t(time.Now().Unix())
	buf.st_mtime = C.time_t(fi.ModTime().Unix())
	buf.st_ctime = C.time_t(fi.ModTime().Unix())

	if fi.IsDir() {
		buf.st_mode = C.short(fi.Mode() | C.S_IFDIR)
	} else {
		buf.st_mode = C.short(fi.Mode() | C.S_IFREG)
	}
}

//export go_shutdown
func go_shutdown() {
	shutDown()
}

//export go_chmod
func go_chmod(path *C.char, mode C.mode_t) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	err := ns.SetAttribute(pp, "mode", os.FileMode(int(mode)))

	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error on chmod of", pp, "(mode =", os.FileMode(int(mode)), "):", err)
	}
	return retVal
}

//export go_truncate
func go_truncate(path *C.char, offset3 C.uint64) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	off := int64(offset3)
	err := ns.SetAttribute(pp, "size", off)

	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error on truncate of", pp, "(size =", off, "):", err)
	}
	return retVal
}

//export go_rename
func go_rename(oldpath *C.char, newpath *C.char) C.int {
	op := pathpkg.Clean("/" + C.GoString(oldpath))
	np := pathpkg.Clean("/" + C.GoString(newpath))

	fi, err := ns.Stat(op)
	if err != nil {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error on rename, stat", op, ":", err)
		}
		return retVal
	}

	err = ns.Move(op, np)
	if err != nil {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error on rename, move", op, "to", np, ":", err)
		}
		return retVal
	}

	fddb.ReplacePath(op, np, fi.IsDir())

	return C.NFS3_OK
}

//export go_modtime
func go_modtime(path *C.char, modtime C.uint32) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	mod := time.Unix(int64(modtime), 0)
	err := ns.SetAttribute(pp, "modtime", mod)

	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error setting modtime (", mod, ") on", pp, ":", err)
	}
	return retVal
}

//export go_create
func go_create(pathname *C.char, mode C.mode_t) C.int {
	pp := pathpkg.Clean("/" + C.GoString(pathname))

	err := ns.CreateFile(pp)
	if err != nil {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error go_create file at create: ", pp, " due to: ", err)
		}
		return retVal
	}

	err = ns.SetAttribute(pp, "mode", os.FileMode(int(mode)))
	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error on go_create file at setmode:", pp, "(mode =", os.FileMode(int(mode)), "):", err)
	}
	return retVal
}

//export go_createover
func go_createover(pathname *C.char, mode C.mode_t) C.int {
	pp := pathpkg.Clean("/" + C.GoString(pathname))

	fi, err := ns.Stat(pp)
	if err == nil {
		if fi.IsDir() {
			fmt.Println("Error go_createover file: ", pp, " due to: Name of a pre-existing directory")
			return C.NFS3ERR_ISDIR
		}

		err = ns.Remove(pp)
		if err != nil {
			retVal, known := errTranslator(err)
			if !known {
				fmt.Println("Error go_createover file at remove: ", pp, " due to: ", err)
			}
			return retVal
		}
	}

	err = ns.CreateFile(pp)
	if err != nil {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error go_createover file at create: ", pp, " due to: ", err)
		}
		return retVal
	}

	err = ns.SetAttribute(pp, "mode", os.FileMode(int(mode)))
	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error on go_createover file at setmode:", pp, "(mode =", os.FileMode(int(mode)), "):", err)
	}
	return retVal
}

//export go_remove
func go_remove(path *C.char) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))

	st, err := ns.Stat(pp)

	if err != nil {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error removing file: ", pp, "\n", err)
		}
		return retVal
	}

	if st.IsDir() {
		//fmt.Println("Error removing file: ", pp, "\n Is a directory.")
		return C.NFS3ERR_ISDIR
	}

	err = ns.Remove(pp)
	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error removing file: ", pp, "\n", err)
	}
	return retVal
}

//export go_rmdir
func go_rmdir(path *C.char) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))

	st, err := ns.Stat(pp)

	if err != nil {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error removing directory: ", pp, "\n", err)
		}
		return retVal
	}

	if !st.IsDir() {
		//fmt.Println("Error removing directory: ", pp, "\n Not a directory.")
		return C.NFS3ERR_NOTDIR
	}

	err = ns.Remove(pp)
	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error removing directory: ", pp, "\n", err)
	}
	return retVal
}

//export go_mkdir
func go_mkdir(path *C.char, mode C.mode_t) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	err := ns.CreateDirectory(pp)

	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error making directory: ", pp, "\n", err)
	}
	return retVal
}

//export go_nop
func go_nop(name *C.char) C.int {
	pp := C.GoString(name)
	fmt.Println("Unsupported Command: ", pp)
	return -1
}

//export go_pwrite
func go_pwrite(path *C.char, buf unsafe.Pointer, count C.u_int, offset C.uint64) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	off := int64(offset)
	counted := int(count)

	//prepare the provided buffer for use
	slice := &reflect.SliceHeader{Data: uintptr(buf), Len: counted, Cap: counted}
	cbuf := *(*[]byte)(unsafe.Pointer(slice))
	copiedBytes, err := ns.WriteFile(pp, cbuf, off)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "eof") {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error on pwrite of", pp, "(start =", off, "count =", counted, "copied =", copiedBytes, "):", err)
		}
		//because a successful pwrite can return any non-negative number
		//we can't return standard NF3 errors (which are all positive)
		//so we send them as a negative to indicate it's an error,
		//and the recipient will have to negative it again to get the original error.
		return -retVal
	}
	return C.int(copiedBytes)

}

//export go_pread
func go_pread(path *C.char, buf unsafe.Pointer, count C.uint32, offset C.uint64) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	off := int64(offset)
	counted := int(count)

	//prepare the provided buffer for use
	slice := &reflect.SliceHeader{Data: uintptr(buf), Len: counted, Cap: counted}
	cbuf := *(*[]byte)(unsafe.Pointer(slice))

	copiedBytes, err := ns.ReadFile(pp, cbuf, off)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "eof") {
		retVal, known := errTranslator(err)
		if !known {
			fmt.Println("Error on pread of", pp, "(start =", off, "count =", counted, "copied =", copiedBytes, "):", err)
		}
		//because a successful pread can return any non-negative number
		//we can't return standard NF3 errors (which are all positive)
		//so we send them as a negative to indicate it's an error,
		//and the recipient will have to negative it again to get the original error.
		return -retVal
	}
	return C.int(copiedBytes)
}

//export go_sync
func go_sync(path *C.char, buf *C.go_statstruct) C.int {
	pp := pathpkg.Clean("/" + C.GoString(path))
	fi, err := ns.Stat(pp)
	retVal, known := errTranslator(err)
	if !known {
		fmt.Println("Error on sync of", pp, ":", err)
	}
	if err == nil {
		statTranslator(fi, fddb.GetFD(pp), buf)
	}
	return retVal
}

type fdCache struct {
	PathMapA   map[string]int
	PathMapB   map[int]string
	FDcounter  int
	FDlistLock *sync.RWMutex
}

func (f *fdCache) GetPath(fd int) (string, error) {
	if fd < 100 {
		return "", os.ErrInvalid
	}
	f.FDlistLock.RLock()
	path, ok := f.PathMapB[fd]
	f.FDlistLock.RUnlock()
	if ok {
		return path, nil
	} else {
		return "", os.ErrInvalid
	}
}

func (f *fdCache) ReplacePath(oldpath, newpath string, isdir bool) {
	f.FDlistLock.Lock()
	fd := f.PathMapA[oldpath]
	delete(f.PathMapA, oldpath)
	delete(f.PathMapB, fd)
	f.PathMapA[newpath] = fd
	f.PathMapB[fd] = newpath

	if isdir {
		op := oldpath + "/"
		np := newpath + "/"
		for path, fh := range f.PathMapA {
			if strings.HasPrefix(path, op) {
				delete(f.PathMapA, path)
				delete(f.PathMapB, fh)
				path = strings.Replace(path, op, np, 1)
				f.PathMapA[path] = fh
				f.PathMapB[fh] = path
			}
		}
	}
	f.FDlistLock.Unlock()
}

func (f *fdCache) GetFD(path string) int {
	f.FDlistLock.RLock()
	i, ok := f.PathMapA[path]
	f.FDlistLock.RUnlock()
	if ok {
		return i
	}
	f.FDlistLock.Lock()
	f.FDcounter++
	newFD := f.FDcounter
	f.PathMapA[path] = newFD
	f.PathMapB[newFD] = path
	f.FDlistLock.Unlock()
	return newFD
}
