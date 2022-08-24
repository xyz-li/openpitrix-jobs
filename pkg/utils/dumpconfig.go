package utils

import (
	"io/ioutil"
	"k8s.io/klog"
	"os"
)

var configPath = "/Users/lihui/go/src/github.com/kubesphere/openpitrix-jobs/kubesphere.yaml"

func DumpConfig() {
	f, err := os.Open(configPath)
	if err != nil {
		klog.Fatalf("open %s failed, error: %s", configPath, err)
	}

	bytes, err := ioutil.ReadAll(f)
	if err != nil {
		klog.Fatalf("read %s failed, error: %s", configPath, err)
	}

	klog.Infof("configmap: %s", string(bytes))
}
