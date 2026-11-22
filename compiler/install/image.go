package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

const (
	chunk = 4 << 20
	// blkflsbuf is the BLKFLSBUF ioctl: drop the kernel's cached copy of a
	// disk, so reading it back reads the disk.
	blkflsbuf = 0x1261
	// blkrrpart is BLKRRPART: re-read the partition table.
	blkrrpart = 0x125f
)

// ImageSum reads a sha256sum file (the release's rollgate-<date>.img.sha256).
func ImageSum(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(b))
	if len(f) == 0 || len(f[0]) != 64 {
		return "", fmt.Errorf("%s: no SHA-256 checksum in it", path)
	}
	if _, err := hex.DecodeString(f[0]); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return strings.ToLower(f[0]), nil
}

// zstdContentSize returns the uncompressed size recorded in a zstd frame
// header (RFC 8878, 3.1.1.1), or 0 if the frame doesn't record it.
func zstdContentSize(h []byte) int64 {
	if len(h) < 6 || binary.LittleEndian.Uint32(h) != 0xFD2FB528 {
		return 0
	}
	fhd := h[4]
	fcsFlag, single, dictFlag := fhd>>6, fhd&0x20 != 0, fhd&3
	at := 5
	if !single {
		at++ // window descriptor
	}
	at += []int{0, 1, 2, 4}[dictFlag]
	size := []int{0, 2, 4, 8}[fcsFlag]
	if fcsFlag == 0 && single {
		size = 1
	}
	if size == 0 || len(h) < at+size {
		return 0
	}
	f := h[at : at+size]
	switch size {
	case 1:
		return int64(f[0])
	case 2:
		return int64(binary.LittleEndian.Uint16(f)) + 256
	case 4:
		return int64(binary.LittleEndian.Uint32(f))
	}
	return int64(binary.LittleEndian.Uint64(f))
}

// WriteImage writes the zstd-compressed image src onto the disk dev, then
// reads it back. Both must match sum, the image's SHA-256. progress, if not
// nil, is called as it goes, with total 0 if the size isn't known.
//
// It first clears the disk's last MiB, so a backup partition table left
// there by an earlier system can't be mistaken for this one's.
func WriteImage(dev, src, sum string, progress func(step string, done, total int64)) error {
	if progress == nil {
		progress = func(string, int64, int64) {}
	}
	var total int64
	if f, err := os.Open(src); err == nil {
		h := make([]byte, 18)
		n, _ := io.ReadFull(f, h)
		total = zstdContentSize(h[:n])
		f.Close()
	}
	// O_EXCL: the kernel refuses if anything has the disk open or mounted.
	disk, err := os.OpenFile(dev, os.O_RDWR|syscall.O_EXCL, 0)
	if err != nil {
		return err
	}
	defer disk.Close()
	end, err := disk.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if total > end {
		return fmt.Errorf("%s holds %s; the image needs %s", dev, Size(end), Size(total))
	}
	if end > 1<<20 && end > total {
		if _, err := disk.WriteAt(make([]byte, 1<<20), end-1<<20); err != nil {
			return fmt.Errorf("clearing the end of %s: %w", dev, err)
		}
	}

	zstd := exec.Command("zstd", "-dcq", "--no-progress", src)
	var zerr bytes.Buffer
	zstd.Stderr = &zerr
	r, err := zstd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := zstd.Start(); err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, chunk)
	var written int64
	for {
		n, rerr := io.ReadFull(r, buf)
		if n > 0 {
			if _, err := disk.WriteAt(buf[:n], written); err != nil {
				_ = zstd.Process.Kill()
				_ = zstd.Wait()
				return fmt.Errorf("writing %s: %w", dev, err)
			}
			h.Write(buf[:n])
			written += int64(n)
			progress("Writing", written, total)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			_ = zstd.Process.Kill()
			_ = zstd.Wait()
			return fmt.Errorf("reading the image: %w", rerr)
		}
		if written > end {
			_ = zstd.Process.Kill()
			_ = zstd.Wait()
			return fmt.Errorf("the image is bigger than %s", dev)
		}
	}
	if err := zstd.Wait(); err != nil {
		return fmt.Errorf("unpacking the image: %v %s", err, strings.TrimSpace(zerr.String()))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("the image on the USB stick is damaged (its checksum is %s, not %s): write the stick again", got, sum)
	}
	if err := disk.Sync(); err != nil {
		return fmt.Errorf("writing %s: %w", dev, err)
	}

	// Read it back from the disk, not from the kernel's cache.
	ioctl(disk, blkflsbuf)
	h.Reset()
	var read int64
	for read < written {
		n := min(int64(chunk), written-read)
		if _, err := disk.ReadAt(buf[:n], read); err != nil {
			return fmt.Errorf("reading %s back: %w", dev, err)
		}
		h.Write(buf[:n])
		read += n
		progress("Checking", read, written)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sum {
		return fmt.Errorf("what %s holds now differs from what was written (checksum %s, not %s): the disk may be failing", dev, got, sum)
	}
	ioctl(disk, blkrrpart)
	return nil
}

// ioctl runs a disk ioctl that takes no argument. On a file that isn't a
// disk (in tests) it fails, which is fine: there is no cache or partition
// table to deal with.
func ioctl(f *os.File, req uintptr) {
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, 0)
}

// linuxRootX86_64 is the GPT type of the router's root partition
// (4F68BCE3-E8CD-4DB1-96E7-FBCAF984B709) as stored on disk.
var linuxRootX86_64 = []byte{0xe3, 0xbc, 0x68, 0x4f, 0xcd, 0xe8, 0xb1, 0x4d, 0x96, 0xe7, 0xfb, 0xca, 0xf9, 0x84, 0xb7, 0x09}

// RootOffset finds the router's root partition on dev (written by
// WriteImage) from the GPT and returns its offset in bytes.
func RootOffset(dev string) (int64, error) {
	f, err := os.Open(dev)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hdr := make([]byte, 92)
	if _, err := f.ReadAt(hdr, 512); err != nil {
		return 0, fmt.Errorf("%s: reading the partition table: %w", dev, err)
	}
	if string(hdr[:8]) != "EFI PART" {
		return 0, fmt.Errorf("%s has no GPT partition table", dev)
	}
	entries := int64(binary.LittleEndian.Uint64(hdr[72:]))
	count := binary.LittleEndian.Uint32(hdr[80:])
	size := int64(binary.LittleEndian.Uint32(hdr[84:]))
	if size < 128 || count > 1024 {
		return 0, fmt.Errorf("%s: unexpected partition table", dev)
	}
	e := make([]byte, size)
	for i := int64(0); i < int64(count); i++ {
		if _, err := f.ReadAt(e, entries*512+i*size); err != nil {
			return 0, fmt.Errorf("%s: reading the partition table: %w", dev, err)
		}
		if bytes.Equal(e[:16], linuxRootX86_64) {
			return int64(binary.LittleEndian.Uint64(e[32:])) * 512, nil
		}
	}
	return 0, errors.New(dev + " has no rollgate root partition")
}
