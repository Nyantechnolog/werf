package build

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/werf/logboek"
	stylePkg "github.com/werf/logboek/pkg/style"
	"github.com/werf/logboek/pkg/types"
	"github.com/werf/werf/pkg/build/image"
	"github.com/werf/werf/pkg/build/import_server"
	"github.com/werf/werf/pkg/build/stage"
	"github.com/werf/werf/pkg/config"
	"github.com/werf/werf/pkg/container_backend"
	"github.com/werf/werf/pkg/git_repo"
	"github.com/werf/werf/pkg/giterminism_manager"
	imagePkg "github.com/werf/werf/pkg/image"
	"github.com/werf/werf/pkg/storage"
	"github.com/werf/werf/pkg/storage/manager"
	"github.com/werf/werf/pkg/util"
	"github.com/werf/werf/pkg/util/parallel"
)

type Conveyor struct {
	werfConfig *config.WerfConfig

	projectDir       string
	containerWerfDir string
	baseTmpDir       string

	baseImagesRepoIdsCache map[string]string
	baseImagesRepoErrCache map[string]error

	sshAuthSock string

	imagesTree *image.ImagesTree

	stageImages        map[string]*stage.StageImage
	giterminismManager giterminism_manager.Interface
	remoteGitRepos     map[string]*git_repo.Remote

	tmpDir string

	ContainerBackend container_backend.ContainerBackend

	StorageLockManager storage.LockManager
	StorageManager     manager.StorageManagerInterface

	onTerminateFuncs []func() error
	importServers    map[string]import_server.ImportServer

	ConveyorOptions

	mutex            sync.Mutex
	serviceRWMutex   map[string]*sync.RWMutex
	stageDigestMutex map[string]*sync.Mutex
}

type ConveyorOptions struct {
	Parallel                        bool
	ParallelTasksLimit              int64
	LocalGitRepoVirtualMergeOptions stage.VirtualMergeOptions

	ImagesToProcess
}

func NewConveyor(werfConfig *config.WerfConfig, giterminismManager giterminism_manager.Interface, projectDir, baseTmpDir, sshAuthSock string, containerBackend container_backend.ContainerBackend, storageManager manager.StorageManagerInterface, storageLockManager storage.LockManager, opts ConveyorOptions) *Conveyor {
	c := &Conveyor{
		werfConfig: werfConfig,

		projectDir:       projectDir,
		containerWerfDir: "/.werf",
		baseTmpDir:       baseTmpDir,

		sshAuthSock: sshAuthSock,

		giterminismManager: giterminismManager,

		stageImages:            make(map[string]*stage.StageImage),
		baseImagesRepoIdsCache: make(map[string]string),
		baseImagesRepoErrCache: make(map[string]error),
		remoteGitRepos:         make(map[string]*git_repo.Remote),
		tmpDir:                 filepath.Join(baseTmpDir, util.GenerateConsistentRandomString(10)),
		importServers:          make(map[string]import_server.ImportServer),

		ContainerBackend:   containerBackend,
		StorageLockManager: storageLockManager,
		StorageManager:     storageManager,

		ConveyorOptions: opts,

		serviceRWMutex:   map[string]*sync.RWMutex{},
		stageDigestMutex: map[string]*sync.Mutex{},
	}

	c.imagesTree = image.NewImagesTree(werfConfig, image.ImagesTreeOptions{
		CommonImageOptions: image.CommonImageOptions{
			Conveyor:           c,
			GiterminismManager: c.GiterminismManager(),
			ContainerBackend:   c.ContainerBackend,
			StorageManager:     c.StorageManager,
			ProjectDir:         c.projectDir,
			ProjectName:        c.ProjectName(),
			ContainerWerfDir:   c.containerWerfDir,
			TmpDir:             c.tmpDir,
		},
		OnlyImages:    opts.OnlyImages,
		WithoutImages: opts.WithoutImages,
	})

	return c
}

func (c *Conveyor) GetServiceRWMutex(service string) *sync.RWMutex {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	rwMutex, ok := c.serviceRWMutex[service]
	if !ok {
		rwMutex = &sync.RWMutex{}
		c.serviceRWMutex[service] = rwMutex
	}

	return rwMutex
}

func (c *Conveyor) UseLegacyStapelBuilder(cr container_backend.ContainerBackend) bool {
	return !cr.HasStapelBuildSupport()
}

func (c *Conveyor) IsBaseImagesRepoIdsCacheExist(key string) bool {
	c.GetServiceRWMutex("BaseImagesRepoIdsCache").RLock()
	defer c.GetServiceRWMutex("BaseImagesRepoIdsCache").RUnlock()

	_, exist := c.baseImagesRepoIdsCache[key]
	return exist
}

func (c *Conveyor) GetBaseImagesRepoIdsCache(key string) string {
	c.GetServiceRWMutex("BaseImagesRepoIdsCache").RLock()
	defer c.GetServiceRWMutex("BaseImagesRepoIdsCache").RUnlock()

	return c.baseImagesRepoIdsCache[key]
}

func (c *Conveyor) SetBaseImagesRepoIdsCache(key, value string) {
	c.GetServiceRWMutex("BaseImagesRepoIdsCache").Lock()
	defer c.GetServiceRWMutex("BaseImagesRepoIdsCache").Unlock()

	c.baseImagesRepoIdsCache[key] = value
}

func (c *Conveyor) IsBaseImagesRepoErrCacheExist(key string) bool {
	c.GetServiceRWMutex("GetBaseImagesRepoErrCache").RLock()
	defer c.GetServiceRWMutex("GetBaseImagesRepoErrCache").RUnlock()

	_, exist := c.baseImagesRepoErrCache[key]
	return exist
}

func (c *Conveyor) GetBaseImagesRepoErrCache(key string) error {
	c.GetServiceRWMutex("GetBaseImagesRepoErrCache").RLock()
	defer c.GetServiceRWMutex("GetBaseImagesRepoErrCache").RUnlock()

	return c.baseImagesRepoErrCache[key]
}

func (c *Conveyor) SetBaseImagesRepoErrCache(key string, err error) {
	c.GetServiceRWMutex("BaseImagesRepoErrCache").Lock()
	defer c.GetServiceRWMutex("BaseImagesRepoErrCache").Unlock()

	c.baseImagesRepoErrCache[key] = err
}

func (c *Conveyor) GetStageDigestMutex(stage string) *sync.Mutex {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	m, ok := c.stageDigestMutex[stage]
	if !ok {
		m = &sync.Mutex{}
		c.stageDigestMutex[stage] = m
	}

	return m
}

func (c *Conveyor) GetLocalGitRepoVirtualMergeOptions() stage.VirtualMergeOptions {
	return c.ConveyorOptions.LocalGitRepoVirtualMergeOptions
}

func (c *Conveyor) GetImportServer(ctx context.Context, imageName, stageName string) (import_server.ImportServer, error) {
	c.GetServiceRWMutex("ImportServer").Lock()
	defer c.GetServiceRWMutex("ImportServer").Unlock()

	importServerName := imageName
	if stageName != "" {
		importServerName += "/" + stageName
	}
	if srv, hasKey := c.importServers[importServerName]; hasKey {
		return srv, nil
	}

	var srv *import_server.RsyncServer

	var stg stage.Interface

	if stageName != "" {
		stg = c.getImageStage(imageName, stageName)
	} else {
		stg = c.GetImage(imageName).GetLastNonEmptyStage()
	}

	if err := c.StorageManager.FetchStage(ctx, c.ContainerBackend, stg); err != nil {
		return nil, fmt.Errorf("unable to fetch stage %s: %w", stg.GetStageImage().Image.Name(), err)
	}

	if err := logboek.Context(ctx).Info().LogProcess(fmt.Sprintf("Firing up import rsync server for image %s", imageName)).
		DoError(func() error {
			var tmpDir string
			if stageName == "" {
				tmpDir = filepath.Join(c.tmpDir, "import-server", imageName)
			} else {
				tmpDir = filepath.Join(c.tmpDir, "import-server", fmt.Sprintf("%s-%s", imageName, stageName))
			}

			if err := os.MkdirAll(tmpDir, os.ModePerm); err != nil {
				return fmt.Errorf("unable to create dir %s: %w", tmpDir, err)
			}

			var dockerImageName string
			if stageName == "" {
				dockerImageName = c.GetImageNameForLastImageStage(imageName)
			} else {
				dockerImageName = c.GetImageNameForImageStage(imageName, stageName)
			}

			var err error
			srv, err = import_server.RunRsyncServer(ctx, dockerImageName, tmpDir)
			if srv != nil {
				c.AppendOnTerminateFunc(func() error {
					if err := srv.Shutdown(ctx); err != nil {
						return fmt.Errorf("unable to shutdown import server %s: %w", srv.DockerContainerName, err)
					}
					return nil
				})
			}
			if err != nil {
				return fmt.Errorf("unable to run rsync import server: %w", err)
			}
			return nil
		}); err != nil {
		return nil, err
	}

	c.importServers[importServerName] = srv

	return srv, nil
}

func (c *Conveyor) AppendOnTerminateFunc(f func() error) {
	c.onTerminateFuncs = append(c.onTerminateFuncs, f)
}

func (c *Conveyor) Terminate(ctx context.Context) error {
	var terminateErrors []error

	for _, onTerminateFunc := range c.onTerminateFuncs {
		if err := onTerminateFunc(); err != nil {
			terminateErrors = append(terminateErrors, err)
		}
	}

	if len(terminateErrors) > 0 {
		errMsg := "Errors occurred during conveyor termination:\n"
		for _, err := range terminateErrors {
			errMsg += fmt.Sprintf(" - %s\n", err)
		}

		// NOTE: Errors printed here because conveyor termination should occur in defer,
		// NOTE: and errors in the defer will be silenced otherwise.
		logboek.Context(ctx).Warn().LogF("%s", errMsg)

		return errors.New(errMsg)
	}

	return nil
}

func (c *Conveyor) GiterminismManager() giterminism_manager.Interface {
	return c.giterminismManager
}

func (c *Conveyor) SetRemoteGitRepo(key string, repo *git_repo.Remote) {
	c.GetServiceRWMutex("RemoteGitRepo").Lock()
	defer c.GetServiceRWMutex("RemoteGitRepo").Unlock()

	c.remoteGitRepos[key] = repo
}

func (c *Conveyor) GetRemoteGitRepo(key string) *git_repo.Remote {
	c.GetServiceRWMutex("RemoteGitRepo").RLock()
	defer c.GetServiceRWMutex("RemoteGitRepo").RUnlock()

	return c.remoteGitRepos[key]
}

type ShouldBeBuiltOptions struct {
	CustomTagFuncList []imagePkg.CustomTagFunc
}

func (c *Conveyor) ShouldBeBuilt(ctx context.Context, opts ShouldBeBuiltOptions) error {
	if err := c.determineStages(ctx); err != nil {
		return err
	}

	phases := []Phase{
		NewBuildPhase(c, BuildPhaseOptions{
			ShouldBeBuiltMode: true,
			BuildOptions: BuildOptions{
				CustomTagFuncList: opts.CustomTagFuncList,
			},
		}),
	}

	if err := c.runPhases(ctx, phases, false); err != nil {
		return err
	}

	return nil
}

func (c *Conveyor) FetchLastImageStage(ctx context.Context, imageName string) error {
	lastImageStage := c.GetImage(imageName).GetLastNonEmptyStage()
	return c.StorageManager.FetchStage(ctx, c.ContainerBackend, lastImageStage)
}

func (c *Conveyor) GetImageInfoGetters(opts imagePkg.InfoGetterOptions) (images []*imagePkg.InfoGetter) {
	for _, img := range c.imagesTree.GetImages() {
		if img.IsArtifact {
			continue
		}

		getter := c.StorageManager.GetImageInfoGetter(img.Name, img.GetLastNonEmptyStage(), opts)
		images = append(images, getter)
	}

	return
}

func (c *Conveyor) GetExportedImagesNames() []string {
	var res []string

	for _, img := range c.imagesTree.GetImages() {
		if img.IsArtifact {
			continue
		}

		res = append(res, img.Name)
	}

	return res
}

func (c *Conveyor) GetImagesEnvArray() []string {
	var envArray []string
	for _, img := range c.imagesTree.GetImages() {
		if img.IsArtifact {
			continue
		}

		envArray = append(envArray, generateImageEnv(img.Name, c.GetImageNameForLastImageStage(img.Name)))
	}

	return envArray
}

func (c *Conveyor) checkContainerBackendSupported(_ context.Context) error {
	if _, isBuildah := c.ContainerBackend.(*container_backend.BuildahBackend); !isBuildah {
		return nil
	}

	var stapelImagesWithAnsible []*config.StapelImage

	for _, img := range c.werfConfig.StapelImages {
		if img.Ansible != nil {
			stapelImagesWithAnsible = append(stapelImagesWithAnsible, img)
		}
	}

	if len(stapelImagesWithAnsible) > 0 {
		var names []string
		for _, img := range stapelImagesWithAnsible {
			names = append(names, fmt.Sprintf("%q", img.GetName()))
		}

		return fmt.Errorf(`Unable to build stapel images [%s], which use ansible builder when buildah container backend is enabled.

Please use shell builder instead, or select docker server backend to continue usage of ansible builder (disable buildah runtime by unsetting WERF_BUILDAH_MODE environment variable).

It is recommended to use shell builder, because ansible builder will be deprecated soon.`, strings.Join(names, ", "))
	}

	return nil
}

func (c *Conveyor) Build(ctx context.Context, opts BuildOptions) error {
	if err := c.checkContainerBackendSupported(ctx); err != nil {
		return err
	}

	if err := c.determineStages(ctx); err != nil {
		return err
	}

	phases := []Phase{
		NewBuildPhase(c, BuildPhaseOptions{
			BuildOptions: opts,
		}),
	}

	return c.runPhases(ctx, phases, true)
}

type ExportOptions struct {
	BuildPhaseOptions  BuildPhaseOptions
	ExportPhaseOptions ExportPhaseOptions
}

func (c *Conveyor) Export(ctx context.Context, opts ExportOptions) error {
	if err := c.determineStages(ctx); err != nil {
		return err
	}

	phases := []Phase{
		NewBuildPhase(c, opts.BuildPhaseOptions),
		NewExportPhase(c, opts.ExportPhaseOptions),
	}

	return c.runPhases(ctx, phases, true)
}

func (c *Conveyor) determineStages(ctx context.Context) error {
	return logboek.Context(ctx).Info().LogProcess("Determining of stages").
		Options(func(options types.LogProcessOptionsInterface) {
			options.Style(stylePkg.Highlight())
		}).
		DoError(func() error {
			return c.doDetermineStages(ctx)
		})
}

func (c *Conveyor) doDetermineStages(ctx context.Context) error {
	if err := c.imagesTree.Calculate(ctx); err != nil {
		return fmt.Errorf("unable to calculate images tree: %w", err)
	}

	return nil
}

func (c *Conveyor) runPhases(ctx context.Context, phases []Phase, logImages bool) error {
	for _, phase := range phases {
		logProcess := logboek.Context(ctx).Debug().LogProcess("Phase %s -- BeforeImages()", phase.Name())
		logProcess.Start()
		if err := phase.BeforeImages(ctx); err != nil {
			logProcess.Fail()
			return fmt.Errorf("phase %s before images handler failed: %w", phase.Name(), err)
		}
		logProcess.End()
	}

	if err := c.doImages(ctx, phases, logImages); err != nil {
		return err
	}

	for _, phase := range phases {
		if err := logboek.Context(ctx).Debug().LogProcess(fmt.Sprintf("Phase %s -- AfterImages()", phase.Name())).
			DoError(func() error {
				if err := phase.AfterImages(ctx); err != nil {
					return fmt.Errorf("phase %s after images handler failed: %w", phase.Name(), err)
				}

				return nil
			}); err != nil {
			return err
		}
	}

	return nil
}

func (c *Conveyor) doImages(ctx context.Context, phases []Phase, logImages bool) error {
	if c.Parallel && len(c.imagesTree.GetImages()) > 1 {
		return c.doImagesInParallel(ctx, phases, logImages)
	} else {
		for _, img := range c.imagesTree.GetImages() {
			if err := c.doImage(ctx, img, phases, logImages); err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *Conveyor) doImagesInParallel(ctx context.Context, phases []Phase, logImages bool) error {
	blockMsg := "Concurrent builds plan"
	if c.ParallelTasksLimit > 0 {
		blockMsg = fmt.Sprintf("%s (no more than %d images at the same time)", blockMsg, c.ParallelTasksLimit)
	}

	logboek.Context(ctx).LogBlock(blockMsg).
		Options(func(options types.LogBlockOptionsInterface) {
			options.Style(stylePkg.Highlight())
		}).
		Do(func() {
			for setId := range c.imagesTree.GetImagesSets() {
				logboek.Context(ctx).LogFHighlight("Set #%d:\n", setId)
				for _, img := range c.imagesTree.GetImagesSets()[setId] {
					logboek.Context(ctx).LogLnHighlight("-", img.LogDetailedName())
				}
				logboek.Context(ctx).LogOptionalLn()
			}
		})

	for setId := range c.imagesTree.GetImagesSets() {
		numberOfTasks := len(c.imagesTree.GetImagesSets()[setId])
		numberOfWorkers := int(c.ParallelTasksLimit)

		if err := parallel.DoTasks(ctx, numberOfTasks, parallel.DoTasksOptions{
			InitDockerCLIForEachWorker: true,
			MaxNumberOfWorkers:         numberOfWorkers,
			LiveOutput:                 true,
		}, func(ctx context.Context, taskId int) error {
			taskImage := c.imagesTree.GetImagesSets()[setId][taskId]

			var taskPhases []Phase
			for _, phase := range phases {
				taskPhases = append(taskPhases, phase.Clone())
			}

			return c.doImage(ctx, taskImage, taskPhases, logImages)
		}); err != nil {
			return err
		}
	}

	return nil
}

func (c *Conveyor) doImage(ctx context.Context, img *image.Image, phases []Phase, logImages bool) error {
	var imagesLogger types.ManagerInterface
	if logImages {
		imagesLogger = logboek.Context(ctx).Default()
	} else {
		imagesLogger = logboek.Context(ctx).Info()
	}

	return imagesLogger.LogProcess(img.LogDetailedName()).
		Options(func(options types.LogProcessOptionsInterface) {
			options.Style(img.LogProcessStyle())
		}).
		DoError(func() error {
			for _, phase := range phases {
				logProcess := logboek.Context(ctx).Debug().LogProcess("Phase %s -- BeforeImageStages()", phase.Name())
				logProcess.Start()
				deferFn, err := phase.BeforeImageStages(ctx, img)
				if deferFn != nil {
					defer deferFn()
				}
				if err != nil {
					logProcess.Fail()
					return fmt.Errorf("phase %s before image %s stages handler failed: %w", phase.Name(), img.GetLogName(), err)
				}
				logProcess.End()

				logProcess = logboek.Context(ctx).Debug().LogProcess("Phase %s -- OnImageStage()", phase.Name())
				logProcess.Start()
				for _, stg := range img.GetStages() {
					logboek.Context(ctx).Debug().LogF("Phase %s -- OnImageStage() %s %s\n", phase.Name(), img.GetLogName(), stg.LogDetailedName())
					if err := phase.OnImageStage(ctx, img, stg); err != nil {
						logProcess.Fail()
						return fmt.Errorf("phase %s on image %s stage %s handler failed: %w", phase.Name(), img.GetLogName(), stg.Name(), err)
					}
				}
				logProcess.End()

				logProcess = logboek.Context(ctx).Debug().LogProcess("Phase %s -- AfterImageStages()", phase.Name())
				logProcess.Start()
				if err := phase.AfterImageStages(ctx, img); err != nil {
					logProcess.Fail()
					return fmt.Errorf("phase %s after image %s stages handler failed: %w", phase.Name(), img.GetLogName(), err)
				}
				logProcess.End()

				logProcess = logboek.Context(ctx).Debug().LogProcess("Phase %s -- ImageProcessingShouldBeStopped()", phase.Name())
				logProcess.Start()
				if phase.ImageProcessingShouldBeStopped(ctx, img) {
					logProcess.End()
					return nil
				}
				logProcess.End()
			}

			return nil
		})
}

func (c *Conveyor) ProjectName() string {
	return c.werfConfig.Meta.Project
}

func (c *Conveyor) GetStageImage(name string) *stage.StageImage {
	c.GetServiceRWMutex("StageImages").RLock()
	defer c.GetServiceRWMutex("StageImages").RUnlock()

	return c.stageImages[name]
}

func (c *Conveyor) UnsetStageImage(name string) {
	c.GetServiceRWMutex("StageImages").Lock()
	defer c.GetServiceRWMutex("StageImages").Unlock()

	delete(c.stageImages, name)
}

func (c *Conveyor) SetStageImage(stageImage *stage.StageImage) {
	c.GetServiceRWMutex("StageImages").Lock()
	defer c.GetServiceRWMutex("StageImages").Unlock()

	c.stageImages[stageImage.Image.Name()] = stageImage
}

func extractLegacyStageImage(stageImage *stage.StageImage) *container_backend.LegacyStageImage {
	if stageImage == nil || stageImage.Image == nil {
		return nil
	}
	return stageImage.Image.(*container_backend.LegacyStageImage)
}

func (c *Conveyor) GetOrCreateStageImage(name string, prevStageImage *stage.StageImage, stg stage.Interface, img *image.Image) *stage.StageImage {
	if stageImage := c.GetStageImage(name); stageImage != nil {
		return stageImage
	}

	i := container_backend.NewLegacyStageImage(extractLegacyStageImage(prevStageImage), name, c.ContainerBackend)

	var baseImage string
	if stg != nil {
		if stg.HasPrevStage() {
			baseImage = prevStageImage.Image.Name()
		} else if stg.IsStapelStage() && stg.Name() == "from" {
			baseImage = prevStageImage.Image.Name()
		} else {
			baseImage = img.GetBaseImageReference()
		}
	}

	stageImage := stage.NewStageImage(c.ContainerBackend, baseImage, i)
	c.SetStageImage(stageImage)
	return stageImage
}

func (c *Conveyor) GetImage(name string) *image.Image {
	for _, img := range c.imagesTree.GetImages() {
		if img.GetName() == name {
			return img
		}
	}

	panic(fmt.Sprintf("Image %q not found!", name))
}

func (c *Conveyor) GetImageStageContentDigest(imageName, stageName string) string {
	return c.getImageStage(imageName, stageName).GetContentDigest()
}

func (c *Conveyor) GetImageContentDigest(imageName string) string {
	return c.GetImage(imageName).GetContentDigest()
}

func (c *Conveyor) getImageStage(imageName, stageName string) stage.Interface {
	if stg := c.GetImage(imageName).GetStage(stage.StageName(stageName)); stg != nil {
		return stg
	} else {
		return c.getLastNonEmptyImageStage(imageName)
	}
}

func (c *Conveyor) getLastNonEmptyImageStage(imageName string) stage.Interface {
	// FIXME: find first existing stage after specified unexisting
	return c.GetImage(imageName).GetLastNonEmptyStage()
}

func (c *Conveyor) FetchImageStage(ctx context.Context, imageName, stageName string) error {
	return c.StorageManager.FetchStage(ctx, c.ContainerBackend, c.getImageStage(imageName, stageName))
}

func (c *Conveyor) FetchLastNonEmptyImageStage(ctx context.Context, imageName string) error {
	return c.StorageManager.FetchStage(ctx, c.ContainerBackend, c.getLastNonEmptyImageStage(imageName))
}

func (c *Conveyor) GetImageNameForLastImageStage(imageName string) string {
	return c.GetImage(imageName).GetLastNonEmptyStage().GetStageImage().Image.Name()
}

func (c *Conveyor) GetImageNameForImageStage(imageName, stageName string) string {
	return c.getImageStage(imageName, stageName).GetStageImage().Image.Name()
}

func (c *Conveyor) GetStageID(imageName string) string {
	return c.GetImage(imageName).GetStageID()
}

func (c *Conveyor) GetImageIDForLastImageStage(imageName string) string {
	return c.GetImage(imageName).GetLastNonEmptyStage().GetStageImage().Image.GetStageDescription().Info.ID
}

func (c *Conveyor) GetImageIDForImageStage(imageName, stageName string) string {
	return c.getImageStage(imageName, stageName).GetStageImage().Image.GetStageDescription().Info.ID
}

func (c *Conveyor) GetImportMetadata(ctx context.Context, projectName, id string) (*storage.ImportMetadata, error) {
	return c.StorageManager.GetStagesStorage().GetImportMetadata(ctx, projectName, id)
}

func (c *Conveyor) PutImportMetadata(ctx context.Context, projectName string, metadata *storage.ImportMetadata) error {
	return c.StorageManager.GetStagesStorage().PutImportMetadata(ctx, projectName, metadata)
}

func (c *Conveyor) RmImportMetadata(ctx context.Context, projectName, id string) error {
	return c.StorageManager.GetStagesStorage().RmImportMetadata(ctx, projectName, id)
}
