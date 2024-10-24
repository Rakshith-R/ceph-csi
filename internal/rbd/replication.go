/*
Copyright 2023 The Ceph-CSI Authors.

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
	"context"
	"fmt"

	librbd "github.com/ceph/go-ceph/rbd"
)

func (rv *rbdVolume) ResyncVol(localStatus librbd.SiteMirrorImageStatus) error {
	if err := rv.resyncImage(); err != nil {
		return fmt.Errorf("failed to resync image: %w", err)
	}

	// If we issued a resync, return a non-final error as image needs to be recreated
	// locally. Caller retries till RBD syncs an initial version of the image to
	// report its status in the resync request.
	return fmt.Errorf("%w: awaiting initial resync due to split brain", ErrUnavailable)
}

// repairResyncedImageID updates the existing image ID with new one.
func (rv *rbdVolume) RepairResyncedImageID(ctx context.Context, ready bool) error {
	// During resync operation the local image will get deleted and a new
	// image is recreated by the rbd mirroring. The new image will have a
	// new image ID. Once resync is completed update the image ID in the OMAP
	// to get the image removed from the trash during DeleteVolume.

	// if the image is not completely resynced skip repairing image ID.
	if !ready {
		return nil
	}
	j, err := volJournal.Connect(rv.Monitors, rv.RadosNamespace, rv.conn.Creds)
	if err != nil {
		return err
	}
	defer j.Destroy()
	// reset the image ID which is stored in the existing OMAP
	return rv.repairImageID(ctx, j, true)
}

func (rv *rbdVolume) DisableVolumeReplication(
	mirroringInfo *librbd.MirrorImageInfo,
	force bool,
) error {
	if !mirroringInfo.Primary {
		// Return success if the below condition is met
		// Local image is secondary
		// Local image is in up+replaying state

		// If the image is in a secondary and its state is  up+replaying means
		// its a healthy secondary and the image is primary somewhere in the
		// remote cluster and the local image is getting replayed. Return
		// success for the Disabling mirroring as we cannot disable mirroring
		// on the secondary image, when the image on the primary site gets
		// disabled the image on all the remote (secondary) clusters will get
		// auto-deleted. This helps in garbage collecting the volume
		// replication Kubernetes artifacts after failback operation.
		localStatus, rErr := rv.GetLocalState()
		if rErr != nil {
			return fmt.Errorf("failed to get local state: %w", rErr)
		}
		if localStatus.Up && localStatus.State == librbd.MirrorImageStatusStateReplaying {
			return nil
		}

		return fmt.Errorf("%w: secondary image status is up=%t and state=%s",
			ErrInvalidArgument, localStatus.Up, localStatus.State)
	}
	err := rv.DisableImageMirroring(force)
	if err != nil {
		return fmt.Errorf("failed to disable image mirroring: %w", err)
	}
	// the image state can be still disabling once we disable the mirroring
	// check the mirroring is disabled or not
	mirroringInfo, err = rv.GetImageMirroringInfo()
	if err != nil {
		return fmt.Errorf("failed to get mirroring info of image: %w", err)
	}
	if mirroringInfo.State == librbd.MirrorImageDisabling {
		return fmt.Errorf("%w: %q is in disabling state", ErrAborted, rv.VolID)
	}

	return nil
}
