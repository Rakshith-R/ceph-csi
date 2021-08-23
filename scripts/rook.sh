#!/bin/bash -E

ROOK_VERSION=${ROOK_VERSION:-"v1.6.6"}
CEPHCSI_IMAGE=${CEPHCSI_IMAGE:-"quay.io/cephcsi/cephcsi:canary"}
ROOK_CEPH_CLUSTER_IMAGE=${ROOK_CEPH_CLUSTER_IMAGE:-"ceph/ceph:v16.2.2"}
ROOK_DEPLOY_TIMEOUT=${ROOK_DEPLOY_TIMEOUT:-300}
ROOK_URL="https://raw.githubusercontent.com/rook/rook/${ROOK_VERSION}/cluster/examples/kubernetes/ceph"
ROOK_BLOCK_POOL_NAME=${ROOK_BLOCK_POOL_NAME:-"newrbdpool"}
KUBECTL_RETRY=5
KUBECTL_RETRY_DELAY=10
VAULT_NS=${NS:-"rook-ceph"}

# trap log_errors ERR

# log_errors is called on exit (see 'trap' above) and tries to provide
# sufficient information to debug deployment problems
function log_errors() {
	# enable verbose execution
	set -x
	kubectl get nodes
	kubectl -n rook-ceph get events
	kubectl -n rook-ceph describe pods
	kubectl -n rook-ceph logs -l app=rook-ceph-operator
	kubectl -n rook-ceph get CephClusters -oyaml
	kubectl -n rook-ceph get CephFilesystems -oyaml
	kubectl -n rook-ceph get CephBlockPools -oyaml

	# this function should not return, a fatal error was caught!
	exit 1
}

rook_version() {
	echo "${ROOK_VERSION#v}" | cut -d'.' -f"${1}"
}

kubectl_retry() {
    local retries=0 action="${1}" ret=0 stdout stderr
    shift

    # temporary files for kubectl output
    stdout=$(mktemp rook-kubectl-stdout.XXXXXXXX)
    stderr=$(mktemp rook-kubectl-stderr.XXXXXXXX)

    while ! kubectl "${action}" "${@}" 2>"${stderr}" 1>"${stdout}"
    do
        # in case of a failure when running "create", ignore errors with "AlreadyExists"
        if [ "${action}" == 'create' ]
        then
            # count lines in stderr that do not have "AlreadyExists"
            ret=$(grep -cvw 'AlreadyExists' "${stderr}")
            if [ "${ret}" -eq 0 ]
            then
                # Success! stderr is empty after removing all "AlreadyExists" lines.
                break
            fi
        fi

        retries=$((retries+1))
        if [ ${retries} -eq ${KUBECTL_RETRY} ]
        then
            ret=1
            break
        fi

	# log stderr and empty the tmpfile
	cat "${stderr}" > /dev/stderr
	true > "${stderr}"
	echo "kubectl_retry ${*} failed, will retry in ${KUBECTL_RETRY_DELAY} seconds"

        sleep ${KUBECTL_RETRY_DELAY}

	# reset ret so that a next working kubectl does not cause a non-zero
	# return of the function
        ret=0
    done

    # write output so that calling functions can consume it
    cat "${stdout}" > /dev/stdout
    cat "${stderr}" > /dev/stderr

    rm -f "${stdout}" "${stderr}"

    return ${ret}
}

function deploy_rook() {

        If rook version is > 1.5 , we will apply CRDs.
        ROOK_MAJOR=$(rook_version 1)
        ROOK_MINOR=$(rook_version 2)
        if  [ "${ROOK_MAJOR}" -eq 1 ] && [ "${ROOK_MINOR}" -ge 5 ];
		then
			kubectl_retry create -f "${ROOK_URL}/crds.yaml"
		fi
        kubectl_retry create -f "${ROOK_URL}/common.yaml"
		TEMP_DIR="$(mktemp -d)"



		ROOK_CEPHCSI_IMAGE="ROOK_CSI_CEPH_IMAGE: \"${CEPHCSI_IMAGE}\""
	    curl -o "${TEMP_DIR}"/operator.yaml "${ROOK_URL}/operator.yaml"
	   	sed -i 's|# CSI_LOG_LEVEL: "0"|CSI_LOG_LEVEL: "5"|g' "${TEMP_DIR}"/operator.yaml
	   	sed -i 's|ROOK_LOG_LEVEL: "INFO"|ROOK_LOG_LEVEL: "DEBUG"|g' "${TEMP_DIR}"/operator.yaml
		sed -i 's|ROOK_CSI_ALLOW_UNSUPPORTED_VERSION: "false"|ROOK_CSI_ALLOW_UNSUPPORTED_VERSION: "true"|g' "${TEMP_DIR}"/operator.yaml
		sed -i "s|# ROOK_CSI_CEPH_IMAGE: \"quay.io/cephcsi/cephcsi:v3.3.1\"|${ROOK_CEPHCSI_IMAGE}|g" "${TEMP_DIR}"/operator.yaml
		sed -i "s|# CSI_ENABLE_OMAP_GENERATOR: \"false\"|CSI_ENABLE_OMAP_GENERATOR: \"true\"|g" "${TEMP_DIR}"/operator.yaml
		sed -i "s|CSI_ENABLE_VOLUME_REPLICATION: \"false\"|CSI_ENABLE_VOLUME_REPLICATION: \"true\"|g" "${TEMP_DIR}"/operator.yaml
		kubectl_retry create -f "${TEMP_DIR}/operator.yaml"
	    ROOK_CEPH_CLUSTER_VERSION_IMAGE_PATH="image: \"${ROOK_CEPH_CLUSTER_IMAGE}\""


        curl -o "${TEMP_DIR}"/cluster-test.yaml "${ROOK_URL}/cluster-test.yaml"
        sed -i "s|image.*|${ROOK_CEPH_CLUSTER_VERSION_IMAGE_PATH}|g" "${TEMP_DIR}"/cluster-test.yaml
		sed -i "s/config: |/config: |\n    \[mon\]\n    mon_warn_on_insecure_global_id_reclaim_allowed = false/g" "${TEMP_DIR}"/cluster-test.yaml
		#cat  "${TEMP_DIR}"/cluster-test.yaml
        kubectl_retry create -f "${TEMP_DIR}/cluster-test.yaml"
        rm -rf "${TEMP_DIR}"

cat <<EOF | kubectl apply -f -
kind: ConfigMap
apiVersion: v1
metadata:
  name: rook-config-override
  namespace: rook-ceph
data:
  config: |
    [global]
    osd_pool_default_size = 1
    mon_warn_on_pool_no_redundancy = false
---
apiVersion: ceph.rook.io/v1
kind: CephCluster
metadata:
  name: my-cluster
  namespace: rook-ceph
spec:
  dataDirHostPath: /var/lib/rook
  cephVersion:
    image: ${ROOK_CEPH_CLUSTER_IMAGE}
    allowUnsupported: true
  mon:
    count: 1
    allowMultiplePerNode: true
  dashboard:
    enabled: true
  crashCollector:
    disable: true
  storage:
    useAllNodes: true
    useAllDevices: true
  network:
    provider: host
  healthCheck:
    daemonHealth:
      mon:
        interval: 45s
        timeout: 600s
EOF
cat <<EOF | kubectl apply -f -
apiVersion: ceph.rook.io/v1
kind: CephBlockPool
metadata:
  name: replicapool
  namespace: rook-ceph
spec:
  replicated:
    size: 1
  mirroring:
    enabled: true
    mode: image
    # schedule(s) of snapshot
    snapshotSchedules:
      - interval: 24h # daily snapshots
        startTime: 14:00:00-05:00
EOF

        kubectl_retry create -f "${ROOK_URL}/toolbox.yaml"
        kubectl_retry create -f "${ROOK_URL}/filesystem-test.yaml"

        # Check if CephCluster is empty
        if ! kubectl_retry -n rook-ceph get cephclusters -oyaml | grep 'items: \[\]' &>/dev/null; then
            check_ceph_cluster_health
        fi

        # Check if CephFileSystem is empty
        if ! kubectl_retry -n rook-ceph get cephfilesystems -oyaml | grep 'items: \[\]' &>/dev/null; then
            check_mds_stat
        fi

        # Check if CephBlockPool is empty
        if ! kubectl_retry -n rook-ceph get cephblockpools -oyaml | grep 'items: \[\]' &>/dev/null; then
            check_rbd_stat ""
        fi
}

function teardown_rook() {
	kubectl delete -f "${ROOK_URL}/pool-test.yaml"
	kubectl delete -f "${ROOK_URL}/filesystem-test.yaml"
	kubectl delete -f "${ROOK_URL}/toolbox.yaml"
	kubectl delete -f "${ROOK_URL}/cluster-test.yaml"
	kubectl delete -f "${ROOK_URL}/operator.yaml"
	kubectl delete -f "${ROOK_URL}/common.yaml"
}

function create_block_pool() {
	curl -o newpool.yaml "${ROOK_URL}/pool-test.yaml"
	sed -i "s/replicapool/$ROOK_BLOCK_POOL_NAME/g" newpool.yaml
	kubectl_retry create -f "./newpool.yaml"
	rm -f "./newpool.yaml"

	check_rbd_stat "$ROOK_BLOCK_POOL_NAME"
}

function delete_block_pool() {
	curl -o newpool.yaml "${ROOK_URL}/pool-test.yaml"
	sed -i "s/replicapool/$ROOK_BLOCK_POOL_NAME/g" newpool.yaml
	kubectl delete -f "./newpool.yaml"
	rm -f "./newpool.yaml"
}

function check_ceph_cluster_health() {
	for ((retry = 0; retry <= ROOK_DEPLOY_TIMEOUT; retry = retry + 5)); do
		echo "Wait for rook deploy... ${retry}s" && sleep 5

		CEPH_STATE=$(kubectl_retry -n rook-ceph get cephclusters -o jsonpath='{.items[0].status.state}')
		CEPH_HEALTH=$(kubectl_retry -n rook-ceph get cephclusters -o jsonpath='{.items[0].status.ceph.health}')
		echo "Checking CEPH cluster state: [$CEPH_STATE]"
		if [ "$CEPH_STATE" = "Created" ]; then
			if [ "$CEPH_HEALTH" = "HEALTH_OK" ]; then
				echo "Creating CEPH cluster is done. [$CEPH_HEALTH]"
				break
			fi
		fi
	done

	if [ "$retry" -gt "$ROOK_DEPLOY_TIMEOUT" ]; then
		echo "[Timeout] CEPH cluster not in a healthy state (timeout)"
		return 1
	fi
	echo ""
}

function check_mds_stat() {
	for ((retry = 0; retry <= ROOK_DEPLOY_TIMEOUT; retry = retry + 5)); do
		FS_NAME=$(kubectl_retry -n rook-ceph get cephfilesystems.ceph.rook.io -ojsonpath='{.items[0].metadata.name}')
		echo "Checking MDS ($FS_NAME) stats... ${retry}s" && sleep 5

		ACTIVE_COUNT=$(kubectl_retry -n rook-ceph get cephfilesystems myfs -ojsonpath='{.spec.metadataServer.activeCount}')

		ACTIVE_COUNT_NUM=$((ACTIVE_COUNT + 0))
		echo "MDS ($FS_NAME) active_count: [$ACTIVE_COUNT_NUM]"
		if ((ACTIVE_COUNT_NUM < 1)); then
			continue
		else
			if kubectl_retry -n rook-ceph get pod -l rook_file_system=myfs | grep Running &>/dev/null; then
				echo "Filesystem ($FS_NAME) is successfully created..."
				break
			fi
		fi
	done

	if [ "$retry" -gt "$ROOK_DEPLOY_TIMEOUT" ]; then
		echo "[Timeout] Failed to get ceph filesystem pods"
		return 1
	fi
	echo ""
}

function check_rbd_stat() {
	for ((retry = 0; retry <= ROOK_DEPLOY_TIMEOUT; retry = retry + 5)); do
		if [ -z "$1" ]; then
			RBD_POOL_NAME=$(kubectl_retry -n rook-ceph get cephblockpools -ojsonpath='{.items[0].metadata.name}')
		else
			RBD_POOL_NAME=$1
		fi
		echo "Checking RBD ($RBD_POOL_NAME) stats... ${retry}s" && sleep 5

		TOOLBOX_POD=$(kubectl_retry -n rook-ceph get pods -l app=rook-ceph-tools -o jsonpath='{.items[0].metadata.name}')
		TOOLBOX_POD_STATUS=$(kubectl_retry -n rook-ceph get pod "$TOOLBOX_POD" -ojsonpath='{.status.phase}')
		[[ "$TOOLBOX_POD_STATUS" != "Running" ]] && \
			{ echo "Toolbox POD ($TOOLBOX_POD) status: [$TOOLBOX_POD_STATUS]"; continue; }

		if kubectl_retry exec -n rook-ceph "$TOOLBOX_POD" -it -- rbd pool stats "$RBD_POOL_NAME" &>/dev/null; then
			echo "RBD ($RBD_POOL_NAME) is successfully created..."
			break
		fi
	done

	if [ "$retry" -gt "$ROOK_DEPLOY_TIMEOUT" ]; then
		echo "[Timeout] Failed to get RBD pool stats"
		return 1
	fi
	echo ""
}

function create_vault() {
	DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
	sed -i "s|vault.default|vault.${VAULT_NS}|g" "$DIR"/../examples/kms/vault/*
	sed -i "s|value: default|value: ${VAULT_NS}|g" "$DIR"/../examples/kms/vault/*
	sed -i "s|value: tenant|value: ${VAULT_NS}|g" "$DIR"/../examples/kms/vault/*
	sed -i "s|namespace: default|namespace: ${VAULT_NS}|g" "$DIR"/../examples/kms/vault/*
	kubectl_retry create -f "$DIR/../examples/kms/vault/csi-kms-connection-details.yaml"
	kubectl_retry create -f "$DIR/../examples/kms/vault/csi-vaulttokenreview-rbac.yaml"
	kubectl_retry  create -f "$DIR/../examples/kms/vault/kms-config.yaml"
	kubectl_retry create -f "$DIR/../examples/kms/vault/tenant-config.yaml"
	kubectl_retry create -f "$DIR/../examples/kms/vault/tenant-token.yaml"
	kubectl_retry create -f "$DIR/../examples/kms/vault/tenant-sa.yaml"
	kubectl_retry create -f "$DIR/../examples/kms/vault/tenant-sa-admin.yaml"
	kubectl_retry create -f "$DIR/../examples/kms/vault/vault.yaml"
	kubectl_retry create -f "$DIR/../examples/kms/vault/vault-psp.yaml"
}

function delete_vault() {
	DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
	kubectl delete -f "$DIR/../examples/kms/vault/csi-kms-connection-details.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/csi-vaulttokenreview-rbac.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/kms-config.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/tenant-config.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/tenant-token.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/tenant-sa.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/tenant-sa-admin.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/vault.yaml"
	kubectl delete -f "$DIR/../examples/kms/vault/vault-psp.yaml"
}

case "${1:-}" in
deploy)
	deploy_rook
	;;
teardown)
	teardown_rook
	;;
create-block-pool)
	create_block_pool
	;;
delete-block-pool)
	delete_block_pool
	;;
vault)
	create_vault
	;;
vault-down)
	delete_vault
	;;
*)
	echo "${ROOK_CEPH_CLUSTER_IMAGE}"
	echo " $0 [command]
Available Commands:
  deploy             Deploy a rook
  teardown           Teardown a rook
  create-block-pool  Create a rook block pool
  delete-block-pool  Delete a rook block pool
" >&2
	;;
esac
