package storage

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"xd/lib/bittorrent"
	"xd/lib/common"
	"xd/lib/log"
	"xd/lib/metainfo"
	"xd/lib/util"
)

// filesystem based storrent storage session
type fsTorrent struct {
	// parent storage
	st *FsStorage
	// infohash
	ih common.Infohash
	// metainfo
	meta *metainfo.TorrentFile
	// cached bitfield
	bf *bittorrent.Bitfield
	// mutex for bitfield access
	bfmtx sync.RWMutex
}

func (t *fsTorrent) AllocateFile(f metainfo.FileInfo) (err error) {
	fname := filepath.Join(t.FilePath(), f.Path.FilePath())
	err = util.EnsureFile(fname, f.Length)
	return
}

func (t *fsTorrent) Allocate() (err error) {
	log.Infof("allocate files for %s", t.meta.TorrentName())
	if t.meta.IsSingleFile() {
		log.Debugf("file is %d bytes", t.meta.Info.Length)
		err = util.EnsureFile(t.FilePath(), t.meta.Info.Length)
	} else {
		for _, f := range t.meta.Info.Files {
			err = t.AllocateFile(f)
			if err != nil {
				break
			}
		}
	}
	return
}

func (t *fsTorrent) Bitfield() *bittorrent.Bitfield {
	t.bfmtx.Lock()
	if t.bf == nil {
		if !t.st.HasBitfield(t.ih) {
			// we have no pieces
			t.st.CreateNewBitfield(t.ih, t.meta.Info.NumPieces())
		}
		t.bf = t.st.FindBitfield(t.ih)
	}
	t.bfmtx.Unlock()
	return t.bf
}

func (t *fsTorrent) DownloadRemaining() (r int64) {
	bf := t.Bitfield()
	have := int64(bf.CountSet()) * int64(t.meta.Info.PieceLength)
	r = t.meta.TotalSize() - have
	return
}

func (t *fsTorrent) MetaInfo() *metainfo.TorrentFile {
	return t.meta
}

func (t *fsTorrent) Name() string {
	return t.meta.TorrentName()
}

func (t *fsTorrent) Infohash() (ih common.Infohash) {
	copy(ih[:], t.ih[:])
	return
}

func (t *fsTorrent) FilePath() string {
	return filepath.Join(t.st.DataDir, t.meta.Info.Path)
}

func (t *fsTorrent) GetPiece(num uint32) (p *common.Piece) {
	sz := t.meta.Info.PieceLength
	pieces := t.meta.Info.NumPieces()
	tl := t.meta.TotalSize()
	if t.meta.IsSingleFile() {
		f, err := os.Open(t.FilePath())
		if err != nil {
			return
		}
		pc := new(common.Piece)
		pc.Index = int64(num)
		idx := pc.Index * int64(sz)
		_, err = f.Seek(idx, 0)
		if err != nil {
			f.Close()
			return
		}

		if num == uint32(pieces-1) {
			pc.Data = make([]byte, tl%int64(sz))
		} else {
			pc.Data = make([]byte, sz)
		}
		_, err = io.ReadFull(f, pc.Data)
		f.Close()
		if err != nil {
			log.Warnf("failed to get piece %d: %s", num, err)
			return nil
		}
		p = pc
	} else {
		pc := new(common.Piece)
		pc.Data = make([]byte, sz)
		pc.Index = int64(num)
		idx := int64(0)
		cur := int64(0)
		left := int64(sz)
		piece_off := int64(sz) * int64(num)
		for _, info := range t.meta.Info.Files {
			if info.Length+cur >= piece_off {
				fpath := filepath.Join(t.FilePath(), info.Path.FilePath())
				f, err := os.Open(fpath)
				if err == nil {
					defer f.Close()
					if info.Length <= left {
						_, err = io.ReadFull(f, pc.Data[idx:idx+info.Length])
						idx += info.Length
						left -= info.Length
						cur += info.Length
						if err != nil {
							p = nil
							log.Errorf("Failed to read %s: %s", fpath, err)
							return
						}
						continue
					} else {
						f.Seek((piece_off-idx)-int64(sz), 0)
						_, err = io.ReadFull(f, pc.Data[idx:left])
						if err != nil {
							p = nil
							log.Errorf("Failed to read %s: %s", fpath, err)
							return
						}
						break
					}
				} else {
					log.Errorf("Failed to open %s: %s", fpath, err)
					return nil
				}
			}
			cur += info.Length
		}
		p = pc

	}
	return
}

func (t *fsTorrent) checkPiece(pc *common.Piece) (err error) {
	if !t.meta.Info.CheckPiece(pc) {
		err = common.ErrInvalidPiece
	}
	return
}

func (t *fsTorrent) PutPiece(pc *common.Piece) error {

	// check integrity
	err := t.checkPiece(pc)
	if err != nil {
		return err
	}
	sz := t.meta.Info.PieceLength
	if t.meta.IsSingleFile() {
		f, err := os.OpenFile(t.FilePath(), os.O_WRONLY, 0640)
		if err != nil {
			log.Errorf("failed to open %s: %s", t.FilePath())
			return err
		}
		idx := pc.Index * int64(sz)
		log.Debugf("seek to %d", idx)
		_, err = f.Seek(idx, 0)
		if err != nil {
			log.Errorf("Failed to seek in %s:, %s", t.FilePath())
			f.Close()
			return err
		}
		_, err = f.Write(pc.Data[:])
		f.Close()
	} else {
		idx := int64(0)
		cur := int64(0)
		left := int64(sz)
		piece_off := int64(sz) * int64(pc.Index)
		for _, info := range t.meta.Info.Files {
			if info.Length+cur >= piece_off {
				fpath := filepath.Join(t.FilePath(), info.Path.FilePath())
				f, err := os.OpenFile(fpath, os.O_WRONLY, 0640)
				if err == nil {
					defer f.Close()
					if info.Length <= left {
						_, err = f.Write(pc.Data[idx : idx+info.Length])
						//err = util.WriteFull(f, pc.Data[idx:idx+info.Length])
						idx += info.Length
						left -= info.Length
						cur += info.Length
						if err != nil {
							log.Errorf("Failed to write %s: %s", fpath, err)
							return err
						}
						continue
					} else {
						f.Seek((piece_off-idx)-int64(sz), 0)
						_, err = f.Write(pc.Data[idx:left])
						// err = util.WriteFull(f, pc.Data[idx:left])
						if err != nil {
							log.Errorf("Failed to write %s: %s", fpath, err)
							return err
						}
						break
					}
				} else {
					log.Errorf("Failed to open %s: %s", fpath, err)
					return err
				}
			}
			cur += info.Length
		}
	}
	// set bitfield
	t.bf.Set(int(pc.Index))
	return nil
}

func (t *fsTorrent) VerifyAll() (err error) {
	t.bfmtx.Lock()
	check := t.st.FindBitfield(t.ih)
	if check == nil {
		// no stored bitfield, calculate it
		log.Infof("no bitfield for %s", t.Name())
		check = bittorrent.NewBitfield(t.meta.Info.NumPieces(), nil).Inverted()
	}
	// verify
	log.Infof("verify local data for %s", t.Name())
	t.bf, err = t.verifyBitfield(check)
	if err == nil {
		if t.bf.Equals(check) {
			log.Infof("%s check okay", t.Name())
		} else {
			log.Infof("%s has miss matched data", t.Name())
		}
	} else {
		t.bfmtx.Unlock()
		return
	}
	t.bfmtx.Unlock()
	err = t.Flush()
	return
}

func (t *fsTorrent) verifyBitfield(bf *bittorrent.Bitfield) (has *bittorrent.Bitfield, err error) {
	pieces := t.meta.Info.NumPieces()
	has = bittorrent.NewBitfield(pieces, nil)
	sz := t.meta.Info.PieceLength
	pc := new(common.Piece)
	pc.Data = make([]byte, sz)
	tl := t.meta.TotalSize()
	if t.meta.IsSingleFile() {
		var f *os.File
		var r int64
		f, err = os.Open(t.FilePath())
		if err != nil {
			log.Errorf("failed to open: %s", err)
			return
		}
		defer f.Close()
		for pc.Index < int64(pieces) {
			var n int
			if pc.Index == int64(pieces-1) {
				// last piece
				idx := tl - r
				pc.Data = make([]byte, idx)
				n, err = io.ReadFull(f, pc.Data)
			} else {
				n, err = io.ReadFull(f, pc.Data)
				if err != nil {
					log.Errorf("verify failed: %s", err)
					return
				}
			}
			r += int64(n)
			if bf.Has(int(pc.Index)) {
				log.Debugf("hash piece %d at %d", pc.Index, r)
				if t.meta.Info.CheckPiece(pc) {
					has.Set(int(pc.Index))
					log.Debugf("piece %d hash okay", pc.Index)
				} else {
					log.Warnf("piece %d hash missmatch", pc.Index)
				}
			}
			pc.Index++
		}
	} else {
		// were we are in the total
		pos := int64(0)
		flen := len(t.meta.Info.Files)
		for fidx, info := range t.meta.Info.Files {
			var f *os.File
			fpath := filepath.Join(t.FilePath(), info.Path.FilePath())
			log.Debugf("open %s", fpath)
			f, err = os.Open(fpath)
			if err == nil {
				left := info.Length
				for left > 0 {

					var n int
					i := pos % int64(sz)
					log.Debugf("%d left pos=%d i=%d", left, pos, i)
					if left >= int64(sz) {
						n, err = io.ReadFull(f, pc.Data[i:])
						pos += int64(n)
					} else {
						log.Debugf("%s straddles piece %d", fpath, pc.Index)
						n, err = io.ReadFull(f, pc.Data[i:i+left])
						pos += int64(n)
						log.Debugf("%d read", n)
						f.Close()
						break
					}
					left -= int64(n)

					if bf.Has(int(pc.Index)) {
						if t.meta.Info.CheckPiece(pc) {
							has.Set(int(pc.Index))
							log.Debugf("piece %d is okay", pc.Index)
						} else {
							log.Warnf("piece %d hash missmatch", pc.Index)
						}
					} else {
						log.Debugf("Don't check %d not in bitfield", pc.Index)
					}

					pc.Index++
				}
			} else {
				log.Errorf("error opening file %s: %s", fpath, err)
			}
			if flen == (fidx+1) && bf.Has(int(pc.Index)) {

				pc.Data = pc.Data[:tl%int64(sz)]
				if t.meta.Info.CheckPiece(pc) {
					has.Set(int(pc.Index))
					log.Debugf("final piece %d is okay", pc.Index)
				} else {
					log.Warnf("final piece %d hash missmatch", pc.Index)
				}
			}
		}
	}
	if err != nil {
		log.Errorf("failed to verify %s: %s", t.Name(), err)
	}
	return
}

func (t *fsTorrent) Flush() error {
	log.Infof("flush bitfield for %s", t.ih.Hex())
	bf := t.Bitfield()
	return t.st.flushBitfield(t.ih, bf)
}

// filesystem based torrent storage
type FsStorage struct {
	// directory for downloaded data
	DataDir string
	// directory for torrent seed data
	MetaDir string
}

func (st *FsStorage) flushBitfield(ih common.Infohash, bf *bittorrent.Bitfield) (err error) {
	fname := st.bitfieldFilename(ih)
	var f *os.File
	f, err = os.OpenFile(fname, os.O_WRONLY|os.O_CREATE, 0600)
	if err == nil {
		err = bf.BEncode(f)
		f.Close()
	}
	return
}

func (st *FsStorage) Init() (err error) {
	log.Info("Ensure filesystem storage")
	if st.DataDir == "" || st.MetaDir == "" {
		err = errors.New("bad FsStorage parameters")
		return
	}
	err = util.EnsureDir(st.DataDir)
	if err == nil {
		err = util.EnsureDir(st.MetaDir)
	}
	return
}

func (st *FsStorage) FindBitfield(ih common.Infohash) (bf *bittorrent.Bitfield) {
	fpath := st.bitfieldFilename(ih)
	f, err := os.Open(fpath)
	if err == nil {
		bf = new(bittorrent.Bitfield)
		err = bf.BDecode(f)
		if err != nil {
			bf = nil
		}
		f.Close()
	}
	return
}

func (st *FsStorage) bitfieldFilename(ih common.Infohash) string {
	return filepath.Join(st.MetaDir, ih.Hex()+".bitfield")
}

func (st *FsStorage) HasBitfield(ih common.Infohash) bool {
	_, err := os.Stat(st.bitfieldFilename(ih))
	return err == nil
}

func (st *FsStorage) CreateNewBitfield(ih common.Infohash, bits int) {
	fname := st.bitfieldFilename(ih)
	bf := bittorrent.NewBitfield(bits, nil)
	f, err := os.OpenFile(fname, os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		bf.BEncode(f)
		f.Close()
	}
}

func (st *FsStorage) OpenTorrent(info *metainfo.TorrentFile) (t Torrent, err error) {
	basepath := filepath.Join(st.DataDir, info.TorrentName())
	if !info.IsSingleFile() {
		// create directory
		os.Mkdir(basepath, 0700)
	}

	ih := info.Infohash()
	metapath := filepath.Join(st.MetaDir, ih.Hex()+".torrent")
	_, err = os.Stat(metapath)

	if os.IsNotExist(err) {
		// put meta info down onto filesystem
		var f *os.File
		f, err = os.OpenFile(metapath, os.O_CREATE|os.O_WRONLY, 0600)
		if err == nil {
			err = info.BEncode(f)
			f.Close()
		}
	}

	if err == nil {
		ft := &fsTorrent{
			st:   st,
			meta: info,
			ih:   ih,
		}
		log.Debugf("allocate space for %s", ft.Name())
		err = ft.Allocate()
		if err != nil {
			t = nil
			return
		}
		t = ft
	}

	return
}

func (st *FsStorage) OpenAllTorrents() (torrents []Torrent, err error) {
	var matches []string
	matches, err = filepath.Glob(filepath.Join(st.MetaDir, "*.torrent"))
	for _, m := range matches {
		var t Torrent
		var f *os.File
		tf := new(metainfo.TorrentFile)
		f, err = os.Open(m)
		if err == nil {
			err = tf.BDecode(f)
			f.Close()
		}
		if err == nil {
			t, err = st.OpenTorrent(tf)
		}
		if t != nil {
			torrents = append(torrents, t)
		}
	}
	return
}

func (st *FsStorage) PollNewTorrents() (torrents []Torrent) {
	matches, _ := filepath.Glob(filepath.Join(st.DataDir, "*.torrent"))
	for _, m := range matches {
		var t Torrent
		tf := new(metainfo.TorrentFile)
		f, err := os.Open(m)
		if err == nil {
			err = tf.BDecode(f)
			f.Close()
		}
		if err != nil {
			log.Warnf("error checking torrent file: %s", err)
		}
		if st.HasBitfield(tf.Infohash()) {
			// we already have this torrent
			continue
		}
		if err == nil {
			t, err = st.OpenTorrent(tf)
		}
		if t != nil {
			torrents = append(torrents, t)
		}
	}
	return
}
