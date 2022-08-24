package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/spf13/cobra"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"io/ioutil"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	v1alpha1 "kubesphere.io/openpitrix-jobs/pkg/apis/application/v1alpha1"
	typedv1alpha1 "kubesphere.io/openpitrix-jobs/pkg/client/clientset/versioned/typed/application/v1alpha1"
	"kubesphere.io/openpitrix-jobs/pkg/constants"
	"kubesphere.io/openpitrix-jobs/pkg/idutils"
	"kubesphere.io/openpitrix-jobs/pkg/s3"
	"os"
	"path"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
	"strings"
)

var builtinKey = "application.kubesphere.io/builtin-app"
var chartDir string
var (
	InvalidScheme = errors.New("invalid scheme")
)

func newImportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "import",
		Short: "import app",
		Run: func(cmd *cobra.Command, args []string) {
			importConfig, err := tryLoadImportConfig()
			if err != nil {
				klog.Fatalf("parse import config failed, error: %s", err)
			}
			s3Client, err := s3.NewS3Client(s3Options)
			if err != nil {
				klog.Fatalf("create s3 client failed, error: %s", err)
			}
			wf := &ImportWorkFlow{
				client:       versionedClient.ApplicationV1alpha1(),
				s3Cleint:     s3Client,
				importConfig: importConfig,
			}

			file, err := os.Open(chartDir)
			if err != nil {
				klog.Fatalf("failed opening directory: %s, error: %s", chartDir, err)
			}
			defer file.Close()

			fileList, err := file.Readdir(0)
			if err != nil {
				klog.Fatalf("read dir failed, error: %s", err)
			}

			for _, fileInfo := range fileList {
				if fileInfo.IsDir() {
					continue
				}
				if !strings.HasSuffix(fileInfo.Name(), ".tgz") {
					klog.Infof("skip file %s", fileInfo.Name())
					continue
				}

				chrt, err := loader.LoadFile(path.Join(chartDir, fileInfo.Name()))
				if err != nil {
					klog.Fatalf("load chart data failed failed, error: %s", err)
				}

				app, err := wf.CreateApp(context.TODO(), chrt)
				if err != nil {
					klog.Fatalf("create chart %s failed, error: %s", chrt.Name(), err)
				}

				appVer, err := wf.CreateAppVer(context.TODO(), app, path.Join(chartDir, fileInfo.Name()))
				if err != nil {
					klog.Errorf("create app version failed, error: %s", err)
				}
				_ = appVer
			}
		},
	}

	f := cmd.Flags()

	f.StringVar(&chartDir, "chart-dir",
		"/root/package",
		"the dir to which charts are saved")

	return cmd
}

type ImportWorkFlow struct {
	client       typedv1alpha1.ApplicationV1alpha1Interface
	s3Cleint     s3.Interface
	importConfig *ImportConfig
}

var _ importInterface = &ImportWorkFlow{}

type importInterface interface {
	CreateApp(ctx context.Context, chrt *chart.Chart) (*v1alpha1.HelmApplication, error)
	CreateCategory(ctx context.Context, name string) (*v1alpha1.HelmCategory, error)
	CreateAppVer(ctx context.Context, app *v1alpha1.HelmApplication, chartFileName string) (*v1alpha1.HelmApplicationVersion, error)
	UpdateAppVersionStatus(ctx context.Context, appVer *v1alpha1.HelmApplicationVersion) (*v1alpha1.HelmApplicationVersion, error)
}

// CreateCategory if create a helm category if category not exists, or it will return that category
func (wf *ImportWorkFlow) CreateCategory(ctx context.Context, name string) (ctg *v1alpha1.HelmCategory, err error) {
	klog.Infof("create category, name: %s", name)
	allCtg, err := wf.client.HelmCategories().List(ctx, metav1.ListOptions{
		LabelSelector: labels.Everything().String(),
	})
	if err != nil {
		klog.Errorf("get all category failed")
		return nil, err
	}

	for ind := range allCtg.Items {
		if allCtg.Items[ind].Spec.Name == name {
			return &allCtg.Items[ind], nil
		}
	}

	ctgId := idutils.GetUuid36(v1alpha1.HelmCategoryIdPrefix)

	desc := wf.importConfig.GetIcon(name)
	if desc == "" {
		desc = "documentation"
	}

	klog.Infof("create category, name: %s, icon: %s", name, desc)
	// create helm category
	ctg = &v1alpha1.HelmCategory{
		ObjectMeta: metav1.ObjectMeta{
			Name: ctgId,
			Annotations: map[string]string{
				constants.CreatorAnnotationKey: "admin",
			},
		},
		Spec: v1alpha1.HelmCategorySpec{
			Name:        name,
			Description: desc,
		},
	}

	return wf.client.HelmCategories().Create(ctx, ctg, metav1.CreateOptions{})
}

func (wf *ImportWorkFlow) CreateApp(ctx context.Context, chrt *chart.Chart) (app *v1alpha1.HelmApplication, err error) {
	klog.Infof("start to create app, chart name: %s, version: %s", chrt.Name(), chrt.Metadata.Version)
	appList, err := wf.client.HelmApplications().List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{builtinKey: "true"}).String(),
	})

	if err != nil {
		klog.Errorf("get application list failed, error: %s", err)
		return nil, err
	}

	for ind := range appList.Items {
		item := &appList.Items[ind]
		if item.GetTrueName() == wf.importConfig.ReplaceAppName(chrt) {
			klog.Infof("helm application exists, name: %s, version: %s", chrt.Name(), chrt.Metadata.Version)
			err = wf.UpdateAppNameInStore(ctx, item, wf.importConfig.ReplaceAppName(chrt))
			if err != nil {
				return nil, err
			}
			return item, nil
		} else if item.GetTrueName() == chrt.Name() {
			// we need update app name
			klog.Infof("chart name: %s, replace name: %s", chrt.Name(), wf.importConfig.ReplaceAppName(chrt))
			app, err := wf.UpdateAppName(ctx, item, wf.importConfig.ReplaceAppName(chrt))
			if err != nil {
				return nil, err
			}
			// update app name in store
			err = wf.UpdateAppNameInStore(ctx, item, wf.importConfig.ReplaceAppName(chrt))
			if err == nil {
				return app, err
			} else {
				return nil, err
			}
		}
	}

	// create category if need
	var ctgName string
	if chrt.Metadata.Annotations != nil && chrt.Metadata.Annotations[constants.CategoryKeyInChart] != "" {
		ctgName = strings.TrimSpace(chrt.Metadata.Annotations[constants.CategoryKeyInChart])
	}

	var ctg *v1alpha1.HelmCategory
	if ctgName != "" {
		ctg, err = wf.CreateCategory(context.TODO(), ctgName)
		if err != nil {
			return nil, err
		}
	}

	appId := idutils.GetUuid36(v1alpha1.HelmApplicationIdPrefix)
	label := map[string]string{
		builtinKey:                  "true",
		constants.WorkspaceLabelKey: constants.SystemWorkspace,
	}
	if ctg != nil {
		label[constants.CategoryIdLabelKey] = ctg.Name
	}

	anno := wf.importConfig.GetExtraAnnotations(chrt)
	anno[constants.CreatorAnnotationKey] = "admin"

	// create helm application
	app = &v1alpha1.HelmApplication{
		ObjectMeta: metav1.ObjectMeta{
			Name:        appId,
			Labels:      label,
			Annotations: anno,
		},
		Spec: v1alpha1.HelmApplicationSpec{
			Name:        wf.importConfig.ReplaceAppName(chrt),
			Description: chrt.Metadata.Description,
			Icon:        chrt.Metadata.Icon,
		},
	}

	return wf.client.HelmApplications().Create(ctx, app, metav1.CreateOptions{})
}

func (wf *ImportWorkFlow) CreateAppVer(ctx context.Context, app *v1alpha1.HelmApplication, chartFileName string) (*v1alpha1.HelmApplicationVersion, error) {
	chrt, err := loader.LoadFile(chartFileName)
	if err != nil {
		klog.Fatalf("load chart data failed failed, error: %s", err)
		return nil, err
	}

	klog.Infof("start to create app version, chart name: %s, version: %s", chrt.Name(), chrt.Metadata.Version)
	chartFile, _ := os.Open(chartFileName)

	appVerList, err := wf.client.HelmApplicationVersions().List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{constants.ChartApplicationIdLabelKey: app.Name}).String(),
	})

	if err != nil {
		klog.Errorf("get application version list failed, error: %s", err)
		return nil, err
	}

	var existsAppVer *v1alpha1.HelmApplicationVersion

	for ind := range appVerList.Items {
		curr := &appVerList.Items[ind]
		if curr.GetChartVersion() == chrt.Metadata.Version {
			klog.Infof("helm application version exists, name: %s, version: %s", curr.GetTrueName(), curr.GetChartVersion())
			if curr.Spec.DataKey != "" && curr.Status.State == v1alpha1.StateActive {
				return existsAppVer, nil
			} else {
				existsAppVer = curr
				break
			}
		}
	}

	var appVerId string
	if existsAppVer == nil {
		appVerId = idutils.GetUuid36(v1alpha1.HelmApplicationVersionIdPrefix)
	} else {
		appVerId = existsAppVer.Name
	}

	// upload chart data
	if existsAppVer == nil || existsAppVer.Spec.DataKey == "" {
		err = wf.s3Cleint.Upload(path.Join(app.GetWorkspace(), appVerId), appVerId, chartFile)
		if err != nil {
			return nil, err
		}
	}

	// create new appVer
	if existsAppVer == nil {
		appVer := &v1alpha1.HelmApplicationVersion{
			ObjectMeta: metav1.ObjectMeta{
				Name: appVerId,
				Labels: map[string]string{
					constants.ChartApplicationIdLabelKey: app.GetHelmApplicationId(),
					constants.WorkspaceLabelKey:          app.GetWorkspace(),
				},
				Annotations: map[string]string{
					constants.CreatorAnnotationKey: "admin",
				},
				OwnerReferences: []metav1.OwnerReference{
					{
						UID:        app.UID,
						Name:       app.Name,
						APIVersion: v1alpha1.SchemeGroupVersion.String(),
						Kind:       v1alpha1.ResourceKindHelmApplication,
					},
				},
			},
			Spec: v1alpha1.HelmApplicationVersionSpec{
				Metadata: &v1alpha1.Metadata{
					Name:        chrt.Name(),
					Version:     chrt.Metadata.Version,
					AppVersion:  chrt.Metadata.AppVersion,
					Icon:        chrt.Metadata.Icon,
					Home:        chrt.Metadata.Home,
					Sources:     chrt.Metadata.Sources,
					Maintainers: translateMaintainers(chrt.Metadata.Maintainers),
					Keywords:    chrt.Metadata.Keywords,
				},
				DataKey: appVerId,
			},
		}

		appVer, err = wf.client.HelmApplicationVersions().Create(ctx, appVer, metav1.CreateOptions{})
		if err != nil {
			klog.Errorf("create helm application version %s failed, error: %s", appVerId, err)
			return nil, err
		}
		klog.Infof("create helm application version %s success", appVerId)
		existsAppVer = appVer
	}

	// update app version status, set state to active
	return wf.UpdateAppVersionStatus(ctx, existsAppVer)
}

func translateMaintainers(mt []*chart.Maintainer) []*v1alpha1.Maintainer {
	ret := make([]*v1alpha1.Maintainer, 0, len(mt))
	for _, value := range mt {
		ret = append(ret, &v1alpha1.Maintainer{
			Name:  value.Name,
			Email: value.Email,
			URL:   value.URL,
		})
	}

	return ret
}

func (wf *ImportWorkFlow) UpdateAppNameInStore(ctx context.Context, app *v1alpha1.HelmApplication, name string) (err error) {
	appId := fmt.Sprintf("%s-%s", app.Name, "store")

	appInStore, err := wf.client.HelmApplications().Get(ctx, appId, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if appInStore.Spec.Name == name {
		return nil
	}

	appInStore, err = wf.UpdateAppName(ctx, appInStore, name)
	return err
}

func (wf *ImportWorkFlow) UpdateAppName(ctx context.Context, oldApp *v1alpha1.HelmApplication, name string) (*v1alpha1.HelmApplication, error) {
	if oldApp.Spec.Name == name {
		return oldApp, nil
	}
	appId := oldApp.Name

	for i := 0; i < 10; i++ {
		patch := client.MergeFrom(oldApp)
		newApp := oldApp.DeepCopy()
		newApp.Spec.Name = name
		data, err := patch.Data(newApp)
		if err != nil {
			return nil, err
		}

		newApp, err = wf.client.HelmApplications().Patch(ctx, oldApp.Name, patch.Type(), data, metav1.PatchOptions{})
		if err != nil {
			klog.Errorf("update app name %s failed, retry: %d, error: %s", oldApp.Name, i, err)
		} else {
			klog.Errorf("update app version %s status success", oldApp.Name)
			return newApp, nil
		}

		// get the exist app
		oldApp, err = wf.client.HelmApplications().Get(ctx, appId, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

	}

	return nil, errors.New("update app name failed")
}

func (wf *ImportWorkFlow) UpdateAppVersionStatus(ctx context.Context, appVer *v1alpha1.HelmApplicationVersion) (*v1alpha1.HelmApplicationVersion, error) {
	klog.Infof("update app version status, chart name: %s, version: %s", appVer.GetTrueName(), appVer.GetChartVersion())
	if appVer.Status.State == v1alpha1.StateActive {
		return appVer, nil
	}

	retry := 5
	var err error
	for i := 0; i < retry; i++ {
		appVer.Status.State = v1alpha1.StateActive
		appVer.Status.Audit = append(appVer.Status.Audit, v1alpha1.Audit{
			State:    v1alpha1.StateActive,
			Time:     metav1.Now(),
			Operator: "admin",
		})

		name := appVer.Name
		appVer, err = wf.client.HelmApplicationVersions().UpdateStatus(ctx, appVer, metav1.UpdateOptions{})
		if err != nil {
			klog.Errorf("update app version %s status failed, retry: %d, error: %s", name, i, err)
		} else {
			klog.Errorf("update app version %s status success", name)
			return appVer, nil
		}
		appVer, err = wf.client.HelmApplicationVersions().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			klog.Errorf("get helm application version %s failed, error: %s", name, err)
			return nil, err
		}
	}

	return appVer, nil
}

type ImportConfig struct {
	// map category name to icon
	CategoryIcon   map[string]string `yaml:"categoryIcon"`
	AppNameReplace map[string]string `yaml:"appNameReplace"`
	// Extra annotations add to a specific chart.
	ExtraAnnotations map[string]map[string]string `yaml:"extraAnnotations"`
}

func (ic *ImportConfig) GetExtraAnnotations(chrt *chart.Chart) map[string]string {
	if anno, exists := ic.ExtraAnnotations[chrt.Name()]; exists {
		return anno
	} else {
		return map[string]string{}
	}
}

func (ic *ImportConfig) ReplaceAppName(chrt *chart.Chart) string {
	// import-config.yaml comes first
	if newName, exists := ic.AppNameReplace[chrt.Name()]; exists {
		return newName
	} else {
		// If app.kubesphere.io/display-name exists in chart's annotation, use this value.
		if chrt.Metadata.Annotations != nil && chrt.Metadata.Annotations[constants.ChartDisplayName] != "" {
			return strings.TrimSpace(chrt.Metadata.Annotations[constants.ChartDisplayName])
		}
		return chrt.Name()
	}
}

func (ic *ImportConfig) GetIcon(ctg string) string {
	if len(ic.CategoryIcon) == 0 {
		return ""
	}

	// viper is case-insensitive
	return ic.CategoryIcon[strings.ToLower(ctg)]
}

func tryLoadImportConfig() (*ImportConfig, error) {
	b, err := ioutil.ReadFile(path.Join(".", "import-config.yaml"))
	if err != nil {
		klog.Errorf("load import-config.yaml failed")
		return nil, err
	}

	conf := &ImportConfig{}

	if err := yaml.Unmarshal(b, conf); err != nil {
		return nil, err
	}

	return conf, nil
}
