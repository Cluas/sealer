// Copyright © 2021 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/alibaba/sealer/common"
	"github.com/alibaba/sealer/image"
	"github.com/alibaba/sealer/image/cache"
	"github.com/alibaba/sealer/image/reference"
	"github.com/alibaba/sealer/image/store"
	"github.com/alibaba/sealer/logger"
	"github.com/alibaba/sealer/parser"
	v1 "github.com/alibaba/sealer/types/api/v1"
	"github.com/alibaba/sealer/utils"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

type Config struct {
	BuildType string
	NoCache   bool
	ImageName string
}

type builderLayer struct {
	baseLayers []v1.Layer
	newLayers  []v1.Layer
}

// LocalBuilder: local builder using local provider to build a cluster image
type LocalBuilder struct {
	Config               *Config
	Image                *v1.Image
	Cluster              *v1.Cluster
	ImageNamed           reference.Named
	ImageID              string
	Context              string
	KubeFileName         string
	LayerStore           store.LayerStore
	ImageStore           store.ImageStore
	ImageService         image.Service
	Prober               image.Prober
	FS                   store.Backend
	DockerImageStorePath string
	builderLayer
}

func (l *LocalBuilder) Build(name string, context string, kubefileName string) error {
	err := l.initBuilder(name, context, kubefileName)
	if err != nil {
		return err
	}

	pipLine, err := l.GetBuildPipeLine()
	if err != nil {
		return err
	}

	for _, f := range pipLine {
		if err = f(); err != nil {
			return err
		}
	}
	return nil
}

func (l *LocalBuilder) initBuilder(name string, context string, kubefileName string) error {
	named, err := reference.ParseToNamed(name)
	if err != nil {
		return err
	}

	l.ImageNamed = named
	l.Context = context
	l.KubeFileName = kubefileName
	return nil
}

func (l *LocalBuilder) GetBuildPipeLine() ([]func() error, error) {
	var buildPipeline []func() error
	if err := l.InitImageSpec(); err != nil {
		return nil, err
	}

	buildPipeline = append(buildPipeline,
		l.PullBaseImageNotExist,
		l.ExecBuild,
		l.UpdateImageMetadata)
	return buildPipeline, nil
}

// init default Image metadata
func (l *LocalBuilder) InitImageSpec() error {
	kubeFile, err := utils.ReadAll(l.KubeFileName)
	if err != nil {
		return fmt.Errorf("failed to load kubefile: %v", err)
	}
	l.Image = parser.NewParse().Parse(kubeFile)
	if l.Image == nil {
		return fmt.Errorf("failed to parse kubefile, image is nil")
	}

	layer0 := l.Image.Spec.Layers[0]
	if layer0.Type != FromCmd {
		return fmt.Errorf("first line of kubefile must start with FROM")
	}

	logger.Info("init image spec success!")
	return nil
}

func (l *LocalBuilder) PullBaseImageNotExist() (err error) {
	if l.Image.Spec.Layers[0].Value == common.ImageScratch {
		return nil
	}
	if err = l.ImageService.PullIfNotExist(l.Image.Spec.Layers[0].Value); err != nil {
		return fmt.Errorf("failed to pull baseImage: %v", err)
	}
	logger.Info("pull base image %s success", l.Image.Spec.Layers[0].Value)
	return nil
}

func (l *LocalBuilder) ExecBuild() error {
	err := l.updateBuilderLayers(l.Image)
	if err != nil {
		return err
	}

	var (
		canUseCache = !l.Config.NoCache
		parentID    = cache.ChainID("")
		newLayers   = l.newLayers
	)

	baseLayerPaths := getBaseLayersPath(l.baseLayers)
	chainSvc, err := cache.NewService()
	if err != nil {
		return err
	}

	hc := handlerContext{
		buildContext:  l.Context,
		continueCache: canUseCache,
		cacheSvc:      chainSvc,
		prober:        l.Prober,
		parentID:      parentID,
	}

	mhandler := handler{
		hc:         hc,
		layerStore: l.LayerStore,
	}
	for i := 0; i < len(newLayers); i++ {
		// take layer reference
		// we are to modify the layer
		layer := &newLayers[i]
		logger.Info("run build layer: %s %s", layer.Type, layer.Value)
		var (
			layerID digest.Digest
			cacheID digest.Digest
			forErr  error
		)

		switch layer.Type {
		case common.CMDCOMMAND, common.RUNCOMMAND:
			layerID, forErr = mhandler.handleCMDRUNCmd(*layer, baseLayerPaths...)
			if forErr != nil {
				return forErr
			}

		case common.COPYCOMMAND:
			layerID, cacheID, forErr = mhandler.handleCopyCmd(*layer)
			if forErr != nil {
				return forErr
			}
			// hit cache failed, so we save cacheID value to metadata cacheID for this layer.
			// and next time, the cacheID will be used to hit cache.
			if layerID != "" && cacheID != "" && !mhandler.hc.continueCache {
				// TODO set cache id under register.
				forErr = l.SetCacheID(layerID, cacheID.String())
				if forErr != nil {
					logger.Warn("set cache failed layer: %v, err: %v", layer, err)
				}
			}
		}

		layer.ID = layerID
		if layerID == "" {
			continue
		}
		baseLayerPaths = append(baseLayerPaths, l.FS.LayerDataDir(layer.ID))
	}
	// todo need to collect docker images while build
	logger.Info("exec all build instructs success !")
	return nil
}

//This function only has meaning for copy layers
func (l *LocalBuilder) SetCacheID(layerID digest.Digest, cID string) error {
	return l.FS.SetMetadata(layerID, cacheID, []byte(cID))
}

func (l *LocalBuilder) squashBaseImageLayerIntoCurrentImage() {
	l.Image.Spec.Layers = append(l.baseLayers, l.newLayers...)
}

func (l *LocalBuilder) registerLayer(tempTarget string) (digest.Digest, error) {
	layerDigest, err := l.LayerStore.RegisterLayerForBuilder(tempTarget)
	if err != nil {
		return "", fmt.Errorf("failed to register layer, err: %v", err)
	}

	return layerDigest, nil
}

func (l *LocalBuilder) UpdateImageMetadata() error {
	l.setClusterFileToImage()
	l.squashBaseImageLayerIntoCurrentImage()
	err := l.updateImageIDAndSaveImage()
	if err != nil {
		return fmt.Errorf("failed to updateImageIDAndSaveImage, err: %v", err)
	}

	logger.Info("update image %s to image metadata success !", l.ImageNamed.Raw())
	return nil
}

// setClusterFileToImage: set cluster file whatever build type is
func (l *LocalBuilder) setClusterFileToImage() {
	clusterFileData := GetRawClusterFile(l.Image)
	l.addImageAnnotations(common.ImageAnnotationForClusterfile, clusterFileData)
}

func (l *LocalBuilder) updateImageIDAndSaveImage() error {
	imageID, err := generateImageID(*l.Image)
	if err != nil {
		return err
	}

	l.Image.Spec.ID = imageID
	return l.ImageStore.Save(*l.Image, l.ImageNamed.Raw())
}

func generateImageID(image v1.Image) (string, error) {
	imageBytes, err := yaml.Marshal(image)
	if err != nil {
		return "", err
	}
	imageID := digest.FromBytes(imageBytes).Hex()
	return imageID, nil
}

// GetClusterFile from user build context or from base image
func GetRawClusterFile(im *v1.Image) string {
	if im.Spec.Layers[0].Value == common.ImageScratch {
		data, err := ioutil.ReadFile(filepath.Join("etc", common.DefaultClusterFileName))
		if err != nil {
			return ""
		}
		return string(data)
	}
	// find cluster file from context
	if clusterFile := getClusterFileFromContext(im); clusterFile != nil {
		logger.Info("get cluster file from context success!")
		return string(clusterFile)
	}
	// find cluster file from base image
	clusterFile := image.GetClusterFileFromImage(im.Spec.Layers[0].Value)
	if clusterFile != "" {
		logger.Info("get cluster file from base image success!")
		return clusterFile
	}
	return ""
}

func getClusterFileFromContext(image *v1.Image) []byte {
	for i := range image.Spec.Layers {
		layer := image.Spec.Layers[i]
		if layer.Type == common.COPYCOMMAND && strings.Fields(layer.Value)[0] == common.DefaultClusterFileName {
			if clusterFile, _ := utils.ReadAll(strings.Fields(layer.Value)[0]); clusterFile != nil {
				return clusterFile
			}
		}
	}
	return nil
}

// GetClusterFile from user build context or from base image
func (l *LocalBuilder) addImageAnnotations(key, value string) {
	if l.Image.Annotations == nil {
		l.Image.Annotations = make(map[string]string)
	}
	l.Image.Annotations[key] = value
}

func (l *LocalBuilder) updateBuilderLayers(image *v1.Image) error {
	// we do not check the len of layers here, because we checked it before.
	// remove the first layer of image
	var (
		layer0    = image.Spec.Layers[0]
		baseImage *v1.Image
		err       error
	)

	// and the layer 0 must be from layer
	if layer0.Value == common.ImageScratch {
		// give a empty image
		baseImage = &v1.Image{}
	} else {
		baseImage, err = l.ImageStore.GetByName(image.Spec.Layers[0].Value)
		if err != nil {
			return fmt.Errorf("failed to get base image while updating base layers, err: %s", err)
		}
	}

	l.baseLayers = append([]v1.Layer{}, baseImage.Spec.Layers...)
	l.newLayers = append([]v1.Layer{}, image.Spec.Layers[1:]...)
	if len(l.baseLayers)+len(l.newLayers) > maxLayerDeep {
		return errors.New("current number of layers exceeds 128 layers")
	}
	return nil
}

// used in build stage, where the image still has from layer
func getBaseLayersPath(layers []v1.Layer) (res []string) {
	for _, layer := range layers {
		if layer.ID != "" {
			res = append(res, filepath.Join(common.DefaultLayerDir, layer.ID.Hex()))
		}
	}
	return res
}

func NewLocalBuilder(config *Config) (Interface, error) {
	layerStore, err := store.NewDefaultLayerStore()
	if err != nil {
		return nil, err
	}

	imageStore, err := store.NewDefaultImageStore()
	if err != nil {
		return nil, err
	}

	service, err := image.NewImageService()
	if err != nil {
		return nil, err
	}

	fs, err := store.NewFSStoreBackend()
	if err != nil {
		return nil, fmt.Errorf("failed to init store backend, err: %s", err)
	}

	prober := image.NewImageProber(service, config.NoCache)

	dockerImageStorePath, err := utils.MkTmpdir()
	if err != nil {
		return nil, fmt.Errorf("failed to create %s:%v", dockerImageStorePath, err)
	}

	return &LocalBuilder{
		Config:               config,
		LayerStore:           layerStore,
		ImageStore:           imageStore,
		ImageService:         service,
		Prober:               prober,
		FS:                   fs,
		DockerImageStorePath: dockerImageStorePath,
		builderLayer: builderLayer{
			// for skip golang ci
			baseLayers: []v1.Layer{},
			newLayers:  []v1.Layer{},
		},
	}, nil
}
