package docker_registry

import (
	"context"
	"fmt"
	"strings"

	"github.com/werf/logboek"
	"github.com/werf/werf/pkg/image"
)

const DefaultImplementationName = "default"

type defaultImplementation struct {
	*api
	Implementation string
}

type defaultImplementationOptions struct {
	apiOptions
}

func newDefaultImplementation(options defaultImplementationOptions) (*defaultImplementation, error) {
	return newDefaultAPIForImplementation(DefaultImplementationName, options)
}

func newDefaultAPIForImplementation(implementation string, options defaultImplementationOptions) (*defaultImplementation, error) {
	d := &defaultImplementation{}
	d.api = newAPI(options.apiOptions)
	d.Implementation = implementation
	return d, nil
}

func (r *defaultImplementation) Tags(ctx context.Context, reference string, _ ...Option) ([]string, error) {
	tags, err := r.api.Tags(ctx, reference)
	if err != nil {
		if IsQuayTagExpiredErr(err) && r.Implementation != QuayImplementationName {
			logboek.Context(ctx).Error().LogF("WARNING: Detected error specific for quay container registry implementation!\n")
			logboek.Context(ctx).Error().LogF("WARNING: Use --repo-container-registry=quay option (or WERF_CONTAINER_REGISTRY env var)\n")
			logboek.Context(ctx).Error().LogF("WARNING:  to instruct werf to use quay driver.\n")
		}

		return []string{}, err
	}

	return tags, nil
}

func (r *defaultImplementation) IsTagExist(_ context.Context, _ string, _ ...Option) (bool, error) {
	panic("not implemented")
}

func (r *defaultImplementation) TryGetRepoImage(ctx context.Context, reference string) (*image.Info, error) {
	info, err := r.api.TryGetRepoImage(ctx, reference)
	if err != nil {
		if IsQuayTagExpiredErr(err) && r.Implementation != QuayImplementationName {
			logboek.Context(ctx).Error().LogF("WARNING: Detected error specific for quay container registry implementation!\n")
			logboek.Context(ctx).Error().LogF("WARNING: Use --repo-container-registry=quay option (or WERF_CONTAINER_REGISTRY env var)\n")
			logboek.Context(ctx).Error().LogF("WARNING:  to instruct werf to use quay driver.\n")
		}

		return nil, err
	}

	return info, nil
}

func (r *defaultImplementation) CreateRepo(_ context.Context, _ string) error {
	return fmt.Errorf("method is not implemented")
}

func (r *defaultImplementation) DeleteRepo(_ context.Context, _ string) error {
	return fmt.Errorf("method is not implemented")
}

func (r *defaultImplementation) TagRepoImage(_ context.Context, repoImage *image.Info, tag string) error {
	return r.api.tagImage(strings.Join([]string{repoImage.Repository, repoImage.Tag}, ":"), tag)
}

func (r *defaultImplementation) DeleteRepoImage(_ context.Context, repoImage *image.Info) error {
	return r.api.deleteImageByReference(strings.Join([]string{repoImage.Repository, repoImage.RepoDigest}, "@"))
}

func (r *defaultImplementation) String() string {
	return DefaultImplementationName
}

func IsManifestUnknownError(err error) bool {
	return (err != nil) && strings.Contains(err.Error(), "MANIFEST_UNKNOWN")
}

func IsNameUnknownError(err error) bool {
	return (err != nil) && strings.Contains(err.Error(), "NAME_UNKNOWN")
}
