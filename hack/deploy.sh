#!/usr/bin/env bash
#
# This file is part of the KubeVirt project
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
#
# Copyright 2017 Red Hat, Inc.
#

source hack/common.sh

HCO_IMAGE=${HCO_IMAGE:-quay.io/kubevirt/hyperconverged-cluster-operator:latest}

# Cleanup previously generated manifests
rm -rf _out/

# Copy release manifests as a base for generated ones, this should make it possible to upgrade
cp -r deploy _out/

# if this is set we run on okd ci
if [ -n "${IMAGE_FORMAT}" ]; then
    component=hyperconverged-cluster-operator
    HCO_IMAGE=`eval echo ${IMAGE_FORMAT}`
fi

sed -i "s#image: quay.io/kubevirt/hyperconverged-cluster-operator:latest#image: ${HCO_IMAGE}#g" _out/operator.yaml

# create namespaces
"${CMD}" create ns kubevirt-hyperconverged

if [ "${CMD}" == "oc" ]; then
    # Switch project to kubevirt
    oc project kubevirt-hyperconverged
else
    # switch namespace to kubevirt
    ${CMD} config set-context $(${CMD} config current-context) --namespace=kubevirt-hyperconverged
fi

CONTAINER_ERRORED=""
function debug(){
    echo "Found pods with errors ${CONTAINER_ERRORED}"

    for err in "${CONTAINER_ERRORED}"; do
	echo "------------- $err"
	"${CMD}" logs $("${CMD}" get pods -n kubevirt-hyperconverged | grep $err | head -1 | awk '{ print $1 }')
    done
    exit 1
}

# Deploy local manifests
"${CMD}" create -f _out/cluster_role.yaml
"${CMD}" create -f _out/service_account.yaml
"${CMD}" create -f _out/cluster_role_binding.yaml
"${CMD}" create -f _out/crds/
"${CMD}" create -f _out/operator.yaml

# Wait for the HCO to be ready
sleep 20

"${CMD}" wait deployment/hyperconverged-cluster-operator --for=condition=Available --timeout="360s" || CONTAINER_ERRORED+="${op}"

for op in cdi-operator cluster-network-addons-operator kubevirt-ssp-operator node-maintenance-operator virt-operator; do
    "${CMD}" wait deployment/"${op}" --for=condition=Available --timeout="360s" || CONTAINER_ERRORED+="${op}"
done

"${CMD}" create -f _out/hco.cr.yaml
sleep 30
"${CMD}" wait pod $("${CMD}" get pods | grep hyperconverged-cluster-operator | awk '{ print $1 }') --for=condition=Ready --timeout="360s"

for dep in cdi-apiserver cdi-deployment cdi-uploadproxy virt-api virt-controller; do
    "${CMD}" wait deployment/"${dep}" --for=condition=Available --timeout="360s" || CONTAINER_ERRORED+="${dep}"
done

if [ -z "$CONTAINER_ERRORED" ]; then
    echo "SUCCESS"
    exit 0
else
    CONTAINER_ERRORED+='hyperconverged-cluster-operator'
    debug
    "${CMD}" get pods -n kubevirt-hyperconverged
fi
