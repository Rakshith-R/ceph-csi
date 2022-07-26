/*
Copyright 2022 The Ceph-CSI Authors.

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

package nodeserver

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	csicommon "github.com/ceph/ceph-csi/internal/csi-common"
	"github.com/ceph/ceph-csi/internal/util"
	"github.com/ceph/ceph-csi/internal/util/log"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	mount "k8s.io/mount-utils"
	netutil "k8s.io/utils/net"
)

const (
	defaultMountPermission = uint64(0o777)
	// Address of the NFS server.
	paramServer           = "server"
	paramShare            = "share"
	paramClusterID        = "clusterid"
	mountOptionsField     = "mountoptions"
	mountPermissionsField = "mountpermissions"
)

// NodeServer struct of ceph CSI driver with supported methods of CSI
// node server spec.
type NodeServer struct {
	*csicommon.DefaultNodeServer
	Mounter mount.Interface
}

// NewNodeServer initialize a node server for ceph CSI driver.
func NewNodeServer(
	d *csicommon.CSIDriver,
	t string,
	topology map[string]string) *NodeServer {
	defaultNodeServer := csicommon.NewDefaultNodeServer(d, t, topology)

	return &NodeServer{
		DefaultNodeServer: defaultNodeServer,
		Mounter:           defaultNodeServer.Mounter,
	}
}

// NodePublishVolume mount the volume.
func (ns *NodeServer) NodePublishVolume(
	ctx context.Context,
	req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	var err error
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument,
			"volume ID missing in request")
	}

	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument,
			"volume capability missing in request")
	}

	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument,
			"target path missing in request")
	}

	// Considering kubelet make sure the stage and publish operations
	// are serialized, we dont need any extra locking in nodePublish.

	mountOptions := volCap.GetMount().GetMountFlags()
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	var server, baseDir, clusterID, netNamespaceFilePath string

	mountPermissions := defaultMountPermission
	performChmodOp := (mountPermissions > 0)
	for k, v := range req.GetVolumeContext() {
		switch strings.ToLower(k) {
		case paramServer:
			server = v
		case paramShare:
			baseDir = v
		case paramClusterID:
			clusterID = v
		case mountOptionsField:
			if v != "" {
				mountOptions = append(mountOptions, v)
			}
		case mountPermissionsField:
			if v != "" {
				var perm uint64
				if perm, err = strconv.ParseUint(v, 8, 32); err != nil {
					return nil, status.Errorf(codes.InvalidArgument,
						"invalid mountPermissions %s: %v", v, err)
				}
				if perm == 0 {
					performChmodOp = false
				} else {
					mountPermissions = perm
				}
			}
		}
	}

	if server == "" {
		return nil, status.Error(codes.InvalidArgument,
			fmt.Sprintf("%v is a required parameter", paramServer))
	}
	if baseDir == "" {
		return nil, status.Error(codes.InvalidArgument,
			fmt.Sprintf("%v is a required parameter", paramShare))
	}
	if netutil.IsIPv6String(server) {
		// if server is IPv6, format to [IPv6].
		server = fmt.Sprintf("[%s]", server)
	}
	source := fmt.Sprintf("%s:%s", server, baseDir)

	if clusterID != "" {
		netNamespaceFilePath, err = util.GetNFSNetNamespaceFilePath(
			util.CsiConfigFile,
			clusterID)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	notMnt, err := ns.Mounter.IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err = os.MkdirAll(targetPath, os.FileMode(mountPermissions)); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if !notMnt {
		log.DebugLog(ctx, "nfs: volume %s is already mounted to %s", volumeID, targetPath)

		return &csi.NodePublishVolumeResponse{}, nil
	}

	log.DefaultLog("nfs: mounting volumeID(%v) source(%s) targetPath(%s) mountflags(%v)", volumeID, source, targetPath, mountOptions)
	err = mountNFS(ctx, source, targetPath, netNamespaceFilePath, mountOptions)
	if err != nil {
		if os.IsPermission(err) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		if strings.Contains(err.Error(), "invalid argument") {
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}

		return nil, status.Error(codes.Internal, err.Error())
	}

	if performChmodOp {
		if err = chmodIfPermissionMismatch(ctx, targetPath, os.FileMode(mountPermissions)); err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
	} else {
		log.DebugLog(ctx, "skipping chmod on targetPath(%s) since mountPermissions is set as 0", targetPath)
	}
	log.DebugLog(ctx, "nfs: successfully mounted volume %q mount %q to %q succeeded", volumeID, source, targetPath)

	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmount the volume.
func (ns *NodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	var err error
	if err := util.ValidateNodeUnpublishVolumeRequest(req); err != nil {
		return nil, err
	}

	// Considering kubelet make sure the stage and publish operations
	// are serialized, we dont need any extra locking in nodePublish.
	volumeID := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	log.DebugLog(ctx, "nfs: unmounting volume %s on %s", volumeID, targetPath)
	err = mount.CleanupMountPoint(targetPath, ns.Mounter, true)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount target %q: %v",
			targetPath, err)
	}
	log.DebugLog(ctx, "nfs: successfully unbounded volume %q from %q",
		volumeID, targetPath)

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeUnstageVolume unstage volume.
func (ns *NodeServer) NodeUnstageVolume(
	ctx context.Context,
	req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeStageVolume stage volume.
func (ns *NodeServer) NodeStageVolume(
	ctx context.Context,
	req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeExpandVolume node expand volume.
func (ns *NodeServer) NodeExpandVolume(
	ctx context.Context,
	req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "")
}

// NodeGetCapabilities returns the supported capabilities of the node server.
func (ns *NodeServer) NodeGetCapabilities(
	ctx context.Context,
	req *csi.NodeGetCapabilitiesRequest,
) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_SINGLE_NODE_MULTI_WRITER,
					},
				},
			},
		},
	}, nil
}

// NodeGetVolumeStats get volume stats.
func (ns *NodeServer) NodeGetVolumeStats(
	ctx context.Context,
	req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	var err error
	targetPath := req.GetVolumePath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument,
			fmt.Sprintf("targetpath %v is empty", targetPath))
	}

	stat, err := os.Stat(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"failed to get stat for targetpath %q: %v", targetPath, err)
	}

	if stat.Mode().IsDir() {
		return csicommon.FilesystemNodeGetVolumeStats(ctx, ns.Mounter, targetPath)
	}

	return nil, status.Errorf(codes.InvalidArgument,
		"targetpath %q is not a directory or device", targetPath)
}

// chmodIfPermissionMismatch only perform chmod when permission mismatches.
func chmodIfPermissionMismatch(
	ctx context.Context,
	targetPath string,
	mode os.FileMode) error {
	info, err := os.Lstat(targetPath)
	if err != nil {
		return err
	}
	perm := info.Mode() & os.ModePerm
	if perm != mode {
		log.DebugLog(ctx, "performing chmod targetPath(%s, mode:0%o) with permissions(0%o)",
			targetPath, info.Mode(), mode)
		if err = os.Chmod(targetPath, mode); err != nil {
			return err
		}
	} else {
		log.DebugLog(ctx, "skipping chmod on targetPath(%s) since mode is already 0%o)",
			targetPath, info.Mode())
	}

	return nil
}

// mountNFS mounts nfs volumes.
func mountNFS(
	ctx context.Context,
	source, mountPoint,
	netNamespaceFilePath string,
	mountOptions []string) error {
	var (
		stderr string
		err    error
	)
	args := []string{
		"-t", "nfs",
		source,
		mountPoint,
	}

	if len(mountOptions) > 0 {
		args = append(append(args, "-o"), mountOptions...)
	}

	if netNamespaceFilePath != "" {
		_, stderr, err = util.ExecuteCommandWithNSEnter(
			ctx, netNamespaceFilePath, "mount", args[:]...)
	} else {
		_, stderr, err = util.ExecCommand(ctx, "mount", args[:]...)
	}
	if err != nil {
		return fmt.Errorf("nfs: failed to mount %q to %q : %v stderr: %s",
			source, mountPoint, err, stderr)
	}

	return err
}
