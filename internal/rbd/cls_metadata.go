/*
Copyright 2026 The Ceph-CSI Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package rbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/ceph/go-ceph/rados"

	rbderrors "github.com/ceph/ceph-csi/internal/rbd/errors"
)

// Image metadata of Ceph-CSI owned keys is written by calling the "rbd"
// object class (cls_rbd) directly on the image header object
// (rbd_header.<imageID>), the same way librbd and rbd-mirror do internally
// via cls_client::metadata_set/metadata_remove.
//
// librbd's metadata_set/metadata_remove go through the exclusive-lock
// machinery: they acquire the exclusive lock (or request the current owner to
// release it). On in-use images, like RWX block PVCs, this causes the lock to
// move between clients and possibly be broken, which (with the default
// rbd_blocklist_on_break_lock=true) results in the previous owner getting
// blocklisted.
//
// The cls based path is lock-free and does not send a header-update notify to
// watchers. That is fine for Ceph-CSI owned keys, as these are only read by
// Ceph-CSI, which opens the image fresh each time. Keys that librbd
// interprets (see isLibrbdManagedMetadataKey) must keep using librbd.

const (
	clsRbd               = "rbd"
	clsMetadataSetMethod = "metadata_set"
	clsMetadataRmMethod  = "metadata_remove"
	rbdHeaderPrefix      = "rbd_header."
)

// clsEncodeString appends the Ceph encoding of a string (or bufferlist) to b:
// a little-endian u32 length followed by the bytes.
func clsEncodeString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))

	return append(b, s...)
}

// clsEncodeMetadataSet returns the Ceph encoding of a map<string, bufferlist>
// with a single key/value pair, as expected by cls_rbd metadata_set.
func clsEncodeMetadataSet(key, value string) []byte {
	in := binary.LittleEndian.AppendUint32(nil, 1)
	in = clsEncodeString(in, key)

	return clsEncodeString(in, value)
}

// isLibrbdManagedMetadataKey returns true for metadata keys that librbd
// validates and applies at runtime (per-image config overrides). These need
// to be set through librbd so that watchers get notified.
func isLibrbdManagedMetadataKey(key string) bool {
	return strings.HasPrefix(key, metadataConfPrefix)
}

// headerObjectName returns the name of the RADOS object that contains the
// header of the image.
func (ri *rbdImage) headerObjectName() (string, error) {
	if ri.ImageID == "" {
		err := ri.getImageID()
		if err != nil {
			return "", err
		}
	}

	return rbdHeaderPrefix + ri.ImageID, nil
}

// clsExecOnHeader runs the given cls_rbd method on the image header object.
// The header object is asserted to exist, so that a deleted image does not
// get an empty header object re-created.
func (ri *rbdImage) clsExecOnHeader(method string, in []byte) error {
	err := ri.openIoctx()
	if err != nil {
		return err
	}

	oid, err := ri.headerObjectName()
	if err != nil {
		return err
	}

	op := rados.CreateWriteOp()
	defer op.Release()

	op.AssertExists()
	op.Exec(clsRbd, method, in)
	err = op.Operate(ri.ioctx, oid, rados.OperationNoFlag)
	if errors.Is(err, rados.ErrNotFound) {
		return fmt.Errorf("Failed as %w (internal %w)", rbderrors.ErrImageNotFound, err)
	}

	return err
}

// clsMetadataSet sets the metadata key/value on the image without acquiring
// the exclusive lock.
func (ri *rbdImage) clsMetadataSet(key, value string) error {
	err := ri.clsExecOnHeader(clsMetadataSetMethod, clsEncodeMetadataSet(key, value))
	if err != nil {
		return fmt.Errorf("failed to set metadata key %q on %q: %w", key, ri, err)
	}

	return nil
}

// clsMetadataRemove removes the metadata key from the image without acquiring
// the exclusive lock. Removing a key that does not exist succeeds.
func (ri *rbdImage) clsMetadataRemove(key string) error {
	err := ri.clsExecOnHeader(clsMetadataRmMethod, clsEncodeString(nil, key))
	if err != nil {
		return fmt.Errorf("failed to remove metadata key %q from %q: %w", key, ri, err)
	}

	return nil
}
