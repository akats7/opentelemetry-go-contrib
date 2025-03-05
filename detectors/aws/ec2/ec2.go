// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package ec2 // import "go.opentelemetry.io/contrib/detectors/aws/ec2"

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

type config struct {
	c client
}

// newConfig returns an appropriately configured config.
func newConfig(options ...Option) *config {
	c := new(config)
	for _, option := range options {
		option.apply(c)
	}

	return c
}

// Option applies an EC2 detector configuration option.
type Option interface {
	apply(*config)
}

type optionFunc func(*config)

func (fn optionFunc) apply(c *config) {
	fn(c)
}

// WithClient sets the ec2metadata client in config.
func WithClient(t client) Option {
	return optionFunc(func(c *config) {
		c.c = t
	})
}

func (cfg *config) getClient() client {
	return cfg.c
}

// resource detector collects resource information from EC2 environment.
type resourceDetector struct {
	c client
}

// Client implements methods to capture EC2 environment metadata information.
//
// Deprecated: Unnecessary public client. This will be removed in a future release.
type Client interface {
	Available() bool
	GetInstanceIdentityDocument() (ec2metadata.EC2InstanceIdentityDocument, error)
	GetMetadata(p string) (string, error)
}

// client implements methods to capture EC2 environment metadata information.
type client interface {
	GetInstanceIdentityDocument(ctx context.Context, params *imds.GetInstanceIdentityDocumentInput, optFns ...func(*imds.Options)) (*imds.GetInstanceIdentityDocumentOutput, error)
	GetMetadata(ctx context.Context, params *imds.GetMetadataInput, optFns ...func(*imds.Options)) (*imds.GetMetadataOutput, error)
}

// compile time assertion that imds.Client implements client.
var _ client = (*imds.Client)(nil)

// compile time assertion that resourceDetector implements the resource.Detector interface.
var _ resource.Detector = (*resourceDetector)(nil)

// NewResourceDetector returns a resource detector that will detect AWS EC2 resources.
func NewResourceDetector(opts ...Option) resource.Detector {
	c := newConfig(opts...)
	return &resourceDetector{c.getClient()}
}

// Detect detects associated resources when running in AWS environment.
func (detector *resourceDetector) Detect(ctx context.Context) (*resource.Resource, error) {
	client, err := detector.client(ctx)
	if err != nil {
		return nil, nil
	}

	// Available method removed in aws-sdk-go-v2, return nil if client returns error
	doc, err := client.GetInstanceIdentityDocument(ctx, nil)
	if err != nil {
		return nil, nil
	}

	attributes := []attribute.KeyValue{
		semconv.CloudProviderAWS,
		semconv.CloudPlatformAWSEC2,
		semconv.CloudRegion(doc.Region),
		semconv.CloudAvailabilityZone(doc.AvailabilityZone),
		semconv.CloudAccountID(doc.AccountID),
		semconv.HostID(doc.InstanceID),
		semconv.HostImageID(doc.ImageID),
		semconv.HostType(doc.InstanceType),
	}

	m := &metadata{client: client}
	m.add(ctx, semconv.HostNameKey, "hostname")

	attributes = append(attributes, m.attributes...)

	if len(m.errs) > 0 {
		err = fmt.Errorf("%w: %s", resource.ErrPartialResource, m.errs)
	}

	return resource.NewWithAttributes(semconv.SchemaURL, attributes...), err
}

func (detector *resourceDetector) client(ctx context.Context) (client, error) {
	if detector.c != nil {
		return detector.c, nil
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	return imds.NewFromConfig(cfg), nil
}

type metadata struct {
	client     client
	errs       []error
	attributes []attribute.KeyValue
}

func (m *metadata) add(ctx context.Context, k attribute.Key, n string) {
	metadataInput := &imds.GetMetadataInput{Path: n}
	md, err := m.client.GetMetadata(ctx, metadataInput)
	if err != nil {
		m.recordError(n, err)
		return
	}
	data, err := io.ReadAll(md.Content)
	if err != nil {
		m.recordError(n, err)
		return
	}
	m.attributes = append(m.attributes, k.String(string(data)))
}

func (m *metadata) recordError(path string, err error) {
	var rf *awshttp.ResponseError
	ok := errors.As(err, &rf)
	if !ok {
		m.errs = append(m.errs, fmt.Errorf("%q: %w", path, err))
		return
	}

	if rf.HTTPStatusCode() == http.StatusNotFound {
		return
	}

	m.errs = append(m.errs, fmt.Errorf("%q: %d %s", path, rf.HTTPStatusCode(), rf.Error()))
}
