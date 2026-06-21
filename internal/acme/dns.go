package acmedns

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/route53/types"
	"golang.org/x/crypto/acme"
)

// Route53Solver implements the ACME DNS-01 challenge using Route 53.
type Route53Solver struct {
	client *route53.Client
	zoneID string
}

// NewRoute53Solver returns a solver that manages TXT records in the
// given Route 53 hosted zone ID.
func NewRoute53Solver(ctx context.Context, zoneID string) (*Route53Solver, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS config: %w", err)
	}

	return &Route53Solver{
		client: route53.NewFromConfig(cfg),
		zoneID: zoneID,
	}, nil
}

// Present creates the DNS TXT record for the ACME challenge.
func (s *Route53Solver) Present(ctx context.Context, challenge *acme.Challenge, domain, token, keyAuth string) error {
	txtValue := keyAuth
	recordName := "_acme-challenge." + domain + "."

	log.Printf("acme: creating TXT record %s", recordName)

	_, err := s.client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(s.zoneID),
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionUpsert,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: aws.String(recordName),
						Type: types.RRTypeTxt,
						TTL:  aws.Int64(60),
						ResourceRecords: []types.ResourceRecord{
							{Value: aws.String(`"` + txtValue + `"`)},
						},
					},
				},
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create TXT record: %w", err)
	}

	// Wait for DNS propagation before Let's Encrypt checks.
	log.Printf("acme: waiting for DNS propagation...")
	time.Sleep(15 * time.Second)

	return nil
}

// CleanUp removes the DNS TXT record after the challenge completes.
func (s *Route53Solver) CleanUp(ctx context.Context, challenge *acme.Challenge, domain, token, keyAuth string) error {
	recordName := "_acme-challenge." + domain + "."

	log.Printf("acme: removing TXT record %s", recordName)

	_, err := s.client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(s.zoneID),
		ChangeBatch: &types.ChangeBatch{
			Changes: []types.Change{
				{
					Action: types.ChangeActionDelete,
					ResourceRecordSet: &types.ResourceRecordSet{
						Name: aws.String(recordName),
						Type: types.RRTypeTxt,
						TTL:  aws.Int64(60),
						ResourceRecords: []types.ResourceRecord{
							{Value: aws.String(`"` + keyAuth + `"`)},
						},
					},
				},
			},
		},
	})
	if err != nil {
		// Log but don't fail — cleanup is best-effort.
		log.Printf("acme: cleanup TXT record: %v", err)
	}

	return nil
}

// GetZoneID looks up the Route 53 hosted zone ID for a given domain.
// Useful so you don't have to hardcode the zone ID.
func GetZoneID(ctx context.Context, domain string) (string, error) {
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return "", fmt.Errorf("load AWS config: %w", err)
	}

	client := route53.NewFromConfig(cfg)

	// Strip to the apex domain for the lookup.
	parts := strings.Split(domain, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("invalid domain: %s", domain)
	}
	apex := strings.Join(parts[len(parts)-2:], ".") + "."

	out, err := client.ListHostedZonesByName(ctx, &route53.ListHostedZonesByNameInput{
		DNSName: aws.String(apex),
	})
	if err != nil {
		return "", fmt.Errorf("list hosted zones: %w", err)
	}

	for _, zone := range out.HostedZones {
		if aws.ToString(zone.Name) == apex {
			// Zone ID comes back as /hostedzone/XXXXX — strip the prefix.
			id := aws.ToString(zone.Id)
			id = strings.TrimPrefix(id, "/hostedzone/")
			return id, nil
		}
	}

	return "", fmt.Errorf("no hosted zone found for %s", domain)
}
