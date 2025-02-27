package framework

import (
	"fmt"
	"strings"

	"github.com/onsi/ginkgo"
	"github.com/onsi/gomega"
	k8sv1 "k8s.io/api/core/v1"
	"kubevirt.io/containerized-data-importer/tests/utils"
)

// CreatePVCFromDefinition is a wrapper around utils.CreatePVCFromDefinition
func (f *Framework) CreatePVCFromDefinition(def *k8sv1.PersistentVolumeClaim) (*k8sv1.PersistentVolumeClaim, error) {
	return utils.CreatePVCFromDefinition(f.K8sClient, f.Namespace.Name, def)
}

// DeletePVC is a wrapper around utils.DeletePVC
func (f *Framework) DeletePVC(pvc *k8sv1.PersistentVolumeClaim) error {
	return utils.DeletePVC(f.K8sClient, f.Namespace.Name, pvc)
}

// WaitForPersistentVolumeClaimPhase is a wrapper around utils.WaitForPersistentVolumeClaimPhase
func (f *Framework) WaitForPersistentVolumeClaimPhase(phase k8sv1.PersistentVolumeClaimPhase, pvcName string) error {
	return utils.WaitForPersistentVolumeClaimPhase(f.K8sClient, f.Namespace.Name, phase, pvcName)
}

// CreateExecutorPodWithPVC is a wrapper around utils.CreateExecutorPodWithPVC
func (f *Framework) CreateExecutorPodWithPVC(podName string, pvc *k8sv1.PersistentVolumeClaim) (*k8sv1.Pod, error) {
	return utils.CreateExecutorPodWithPVC(f.K8sClient, podName, f.Namespace.Name, pvc)
}

// FindPVC is a wrapper around utils.FindPVC
func (f *Framework) FindPVC(pvcName string) (*k8sv1.PersistentVolumeClaim, error) {
	return utils.FindPVC(f.K8sClient, f.Namespace.Name, pvcName)
}

// VerifyPVCIsEmpty verifies a passed in PVC is empty, returns true if the PVC is empty, false if it is not.
func VerifyPVCIsEmpty(f *Framework, pvc *k8sv1.PersistentVolumeClaim) (bool, error) {
	executorPod, err := f.CreateExecutorPodWithPVC("verify-pvc-empty", pvc)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	err = f.WaitTimeoutForPodReady(executorPod.Name, utils.PodWaitForTime)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	output, err := f.ExecShellInPod(executorPod.Name, f.Namespace.Name, "ls -1 /pvc | wc -l")
	if err != nil {
		return false, err
	}
	return strings.Compare("0", output) == 0, nil
}

// CreateAndPopulateSourcePVC Creates and populates a PVC using the provided POD and command
func (f *Framework) CreateAndPopulateSourcePVC(pvcDef *k8sv1.PersistentVolumeClaim, podName string, fillCommand string) *k8sv1.PersistentVolumeClaim {
	// Create the source PVC and populate it with a file, so we can verify the clone.
	sourcePvc, err := f.CreatePVCFromDefinition(pvcDef)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	pod, err := f.CreatePod(utils.NewPodWithPVC(podName, fillCommand, sourcePvc))
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	err = f.WaitTimeoutForPodStatus(pod.Name, k8sv1.PodSucceeded, utils.PodWaitForTime)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	return sourcePvc
}

// VerifyTargetPVCContent is used to check the contents of a PVC and ensure it matches the provided expected data
func (f *Framework) VerifyTargetPVCContent(namespace *k8sv1.Namespace, pvc *k8sv1.PersistentVolumeClaim,
	expectedData, testBaseDir, testFile string) (bool, error) {

	var dest string
	var err error
	var executorPod *k8sv1.Pod

	executorPod, err = utils.CreateExecutorPodWithPVC(f.K8sClient, "verify-pvc-content", namespace.Name, pvc)
	volumeMode := pvc.Spec.VolumeMode
	if volumeMode != nil && *volumeMode == k8sv1.PersistentVolumeBlock {
		dest = testBaseDir
	} else {
		dest = testBaseDir + testFile
	}

	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	err = utils.WaitTimeoutForPodReady(f.K8sClient, executorPod.Name, namespace.Name, utils.PodWaitForTime)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	output, err := f.ExecShellInPod(executorPod.Name, namespace.Name, "cat "+dest)
	if err != nil {
		return false, err
	}
	return strings.Compare(expectedData, output) == 0, nil
}

// VerifyTargetPVCContentMD5 provides a function to check the md5 of data on a PVC and ensure it matches that which is provided
func (f *Framework) VerifyTargetPVCContentMD5(namespace *k8sv1.Namespace, pvc *k8sv1.PersistentVolumeClaim, fileName string, expectedHash string, numBytes ...int64) (bool, error) {
	if len(numBytes) == 0 {
		numBytes = append(numBytes, 0)
	}

	md5, err := f.GetMD5(namespace, pvc, fileName, numBytes[0])
	if err != nil {
		return false, err
	}

	return expectedHash == md5, nil
}

// GetMD5 returns the MD5 of a file on a PVC
func (f *Framework) GetMD5(namespace *k8sv1.Namespace, pvc *k8sv1.PersistentVolumeClaim, fileName string, numBytes int64) (string, error) {
	var executorPod *k8sv1.Pod
	var err error

	executorPod, err = utils.CreateExecutorPodWithPVC(f.K8sClient, "get-md5-"+pvc.Name, namespace.Name, pvc)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	err = utils.WaitTimeoutForPodReady(f.K8sClient, executorPod.Name, namespace.Name, utils.PodWaitForTime)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())

	cmd := "md5sum " + fileName
	if numBytes > 0 {
		cmd = fmt.Sprintf("head -c %d %s | md5sum", numBytes, fileName)
	}

	output, err := f.ExecShellInPod(executorPod.Name, namespace.Name, cmd)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(ginkgo.GinkgoWriter, "INFO: md5sum found %s\n", string(output[:32]))
	return output[:32], nil
}

// RunCommandAndCaptureOutput runs a command on a pod that has the passed in PVC mounted and captures the output.
func (f *Framework) RunCommandAndCaptureOutput(pvc *k8sv1.PersistentVolumeClaim, cmd string) (string, error) {
	executorPod, err := f.CreateExecutorPodWithPVC("execute-command", pvc)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	err = f.WaitTimeoutForPodReady(executorPod.Name, utils.PodWaitForTime)
	gomega.Expect(err).ToNot(gomega.HaveOccurred())
	output, err := f.ExecShellInPod(executorPod.Name, f.Namespace.Name, cmd)
	if err != nil {
		return "", err
	}
	return output, nil
}
