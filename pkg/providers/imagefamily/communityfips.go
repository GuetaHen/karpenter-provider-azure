/*
Portions Copyright (c) Microsoft Corporation.

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

package imagefamily

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	sdkerrors "github.com/Azure/azure-sdk-for-go-extensions/pkg/errors"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute/v7"
	"github.com/Azure/karpenter-provider-azure/pkg/apis/v1beta1"
	"github.com/Azure/karpenter-provider-azure/pkg/operator/options"
	types "github.com/Azure/karpenter-provider-azure/pkg/providers/imagefamily/types"
	"github.com/samber/lo"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// FIPSImagesNotFoundReason is the ImagesReady reason when a region of a FIPS NodeClass has no usable
	// FIPS node image in the public community gallery.
	FIPSImagesNotFoundReason = "FIPSImagesNotFound"
	// CommunityGalleryFIPSImagesUnavailableReason is the ImagesReady reason when the image family of a
	// FIPS NodeClass publishes no usable FIPS node image in the public community gallery.
	CommunityGalleryFIPSImagesUnavailableReason = "CommunityGalleryFIPSImagesUnavailable"
	// PartialFIPSImageListExpiration is how long a community gallery FIPS image listing that skipped an image
	// definition is cached, instead of ImageExpirationInterval, so that a definition missing only briefly
	// returns soon.
	PartialFIPSImageListExpiration = 5 * time.Minute
)

// HasCommunityGalleryFIPSImages reports whether the image family of nodeClass publishes FIPS node images
// in the public community gallery. Only Azure Linux does; the Ubuntu gallery lists a FIPS image
// definition whose only version is a placeholder.
func HasCommunityGalleryFIPSImages(nodeClass *v1beta1.AKSNodeClass) bool {
	return lo.FromPtr(nodeClass.Spec.ImageFamily) == v1beta1.AzureLinuxImageFamily
}

// listsCommunityGalleryFIPSImages reports whether the FIPS node images of nodeClass come from the public
// community gallery: --community-gallery-fips-images is set, --use-sig is not, nodeClass requests FIPS
// and its image family publishes FIPS images there.
func listsCommunityGalleryFIPSImages(opts *options.Options, nodeClass *v1beta1.AKSNodeClass) bool {
	return opts.CommunityGalleryFIPSImages && !opts.UseSIG &&
		lo.FromPtr(nodeClass.Spec.FIPSMode) == v1beta1.FIPSModeFIPS && HasCommunityGalleryFIPSImages(nodeClass)
}

// listsSIGImageDefinitions reports whether DefaultImages must return the image definitions that it lists
// for SIG: with UseSIG, and for community gallery FIPS images, which DefaultImages lists only for SIG.
// Each of those definitions also names its community gallery.
func listsSIGImageDefinitions(ctx context.Context, nodeClass *v1beta1.AKSNodeClass) bool {
	opts := options.FromContext(ctx)
	return opts.UseSIG || listsCommunityGalleryFIPSImages(opts, nodeClass)
}

// FIPSImagesNotFoundError reports a region in which the public community gallery has no usable version
// of any FIPS image definition that a NodeClass needs.
type FIPSImagesNotFoundError struct {
	Location         string
	ImageDefinitions []string
}

func (e *FIPSImagesNotFoundError) Error() string {
	return fmt.Sprintf("the public community gallery has no usable FIPS node image in region %s (image definitions %s)",
		e.Location, strings.Join(e.ImageDefinitions, ", "))
}

// listCommunityFIPS resolves FIPS node images from the public community gallery in p.location, each image
// definition at its newest usable version. A definition without one is skipped, as listSIG skips one that
// the node image versions API does not list, so instance types that only it serves cannot launch in this
// region, and List caches the listing for PartialFIPSImageListExpiration only. A region without any of the
// definitions fails; there is no fallback to non-FIPS images.
func (p *provider) listCommunityFIPS(ctx context.Context, supportedImages []types.DefaultImageOutput) ([]NodeImage, error) {
	nodeImages := []NodeImage{}
	var missing []string
	for _, supportedImage := range supportedImages {
		version, err := p.latestUsableCommunityVersion(ctx, supportedImage.PublicGalleryURL, supportedImage.ImageDefinition)
		if err != nil {
			return nil, err
		}
		if version == "" {
			missing = append(missing, supportedImage.ImageDefinition)
			continue
		}
		nodeImages = append(nodeImages, NodeImage{
			ID:           BuildImageIDCIG(supportedImage.PublicGalleryURL, supportedImage.ImageDefinition, version),
			Requirements: supportedImage.Requirements,
		})
	}
	if len(missing) == 0 {
		return nodeImages, nil
	}
	if len(nodeImages) == 0 {
		return nil, &FIPSImagesNotFoundError{Location: p.location, ImageDefinitions: missing}
	}
	log.FromContext(ctx).Info("FIPS image definitions without a usable community gallery version are skipped in this region",
		"region", p.location, "imageDefinitions", missing)
	return nodeImages, nil
}

// latestUsableCommunityVersion returns the newest published version of an image definition in p.location,
// or "" when it has none. It never selects a placeholder version such as 0.0.0 or a version excluded from
// latest. A definition that the location does not publish has no version.
func (p *provider) latestUsableCommunityVersion(ctx context.Context, publicGalleryURL, imageDefinition string) (string, error) {
	pager := p.imageVersionsClient.NewListPager(p.location, publicGalleryURL, imageDefinition, nil)
	var newest *armcompute.CommunityGalleryImageVersion
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if sdkerrors.IsNotFoundErr(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("listing versions of community gallery image %s in %s: %w", imageDefinition, p.location, err)
		}
		for _, version := range page.Value {
			if usableCommunityVersion(version) && (newest == nil || publishedDate(version).After(publishedDate(newest))) {
				newest = version
			}
		}
	}
	if newest == nil {
		return "", nil
	}
	return *newest.Name, nil
}

func usableCommunityVersion(version *armcompute.CommunityGalleryImageVersion) bool {
	if version == nil || lo.FromPtr(version.Name) == "" || isPlaceholderImageVersion(*version.Name) {
		return false
	}
	return version.Properties == nil || !lo.FromPtr(version.Properties.ExcludeFromLatest)
}

func publishedDate(version *armcompute.CommunityGalleryImageVersion) time.Time {
	if version.Properties == nil {
		return time.Time{}
	}
	return lo.FromPtr(version.Properties.PublishedDate)
}

// isPlaceholderImageVersion reports a version whose numeric parts are all zero, such as 0.0.0, which a
// gallery can publish for an image definition without a node image.
func isPlaceholderImageVersion(name string) bool {
	for _, part := range strings.Split(name, ".") {
		if n, err := strconv.Atoi(part); err != nil || n != 0 {
			return false
		}
	}
	return true
}
