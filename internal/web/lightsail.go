package web

import (
	"net/http"
	"strings"

	"reladock/internal/cloud/lightsail"
	"reladock/internal/platform"
)

type LightsailOptions struct {
	Config   lightsail.Config
	Provider lightsail.Provider
}

func (s *server) serveLightsail(w http.ResponseWriter, r *http.Request, who platform.Identity) bool {
	const root = "/api/admin/lightsail"
	if r.URL.Path != root && !strings.HasPrefix(r.URL.Path, root+"/") {
		return false
	}
	// Catalog and status have no mutating store call, so use the same
	// platform-admin read path as listing before consulting the provider.
	instances, err := s.store.AdminLightsailInstances(r.Context(), who)
	if err != nil {
		failure(w, err)
		return true
	}
	configured := s.lightsail != nil && s.lightsail.Config.Enabled && s.lightsail.Provider != nil

	if r.URL.Path == root+"/status" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return true
		}
		if len(r.URL.Query()) != 0 {
			failure(w, platform.ErrInvalid)
			return true
		}
		if !configured {
			respond(w, http.StatusOK, map[string]bool{"enabled": false})
			return true
		}
		respond(w, http.StatusOK, struct {
			AllowedRegions []string `json:"allowedRegions"`
			Enabled        bool     `json:"enabled"`
		}{AllowedRegions: append([]string(nil), s.lightsail.Config.AllowedRegions...), Enabled: true})
		return true
	}
	if !configured {
		respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Lightsail 连接器未配置"})
		return true
	}

	if r.URL.Path == root+"/catalog" {
		if r.Method != http.MethodGet {
			methodNotAllowed(w)
			return true
		}
		query := r.URL.Query()
		regions, ok := query["region"]
		if !ok || len(query) != 1 || len(regions) != 1 || !s.lightsail.Config.AllowsRegion(regions[0]) {
			failure(w, platform.ErrInvalid)
			return true
		}
		catalog, providerErr := s.lightsail.Provider.Catalog(r.Context(), regions[0])
		if providerErr != nil {
			respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Lightsail 服务暂时不可用"})
			return true
		}
		respond(w, http.StatusOK, filterLightsailCatalog(catalog))
		return true
	}

	if r.URL.Path == root+"/instances" {
		if len(r.URL.Query()) != 0 {
			failure(w, platform.ErrInvalid)
			return true
		}
		switch r.Method {
		case http.MethodGet:
			respond(w, http.StatusOK, instances)
		case http.MethodPost:
			key := r.Header.Get("Idempotency-Key")
			if key == "" {
				failure(w, platform.ErrInvalid)
				return true
			}
			var input platform.LightsailCreateInput
			if !decodeLimit(w, r, &input, 96*1024) {
				return true
			}
			if !s.lightsail.Config.AllowsRegion(input.Selection.Region) {
				failure(w, platform.ErrInvalid)
				return true
			}
			catalog, providerErr := s.lightsail.Provider.Catalog(r.Context(), input.Selection.Region)
			if providerErr != nil {
				respond(w, http.StatusServiceUnavailable, map[string]string{"error": "Lightsail 服务暂时不可用"})
				return true
			}
			if !selectionInCatalog(input.Selection, filterLightsailCatalog(catalog)) {
				failure(w, platform.ErrInvalid)
				return true
			}
			created, createErr := s.store.AdminCreateLightsailInstance(r.Context(), who, key, input)
			if createErr != nil {
				failure(w, createErr)
				return true
			}
			respond(w, http.StatusAccepted, created)
		default:
			methodNotAllowed(w)
		}
		return true
	}

	remainder := strings.TrimPrefix(r.URL.Path, root+"/instances/")
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || !uuid.MatchString(parts[0]) || (parts[1] != "stop" && parts[1] != "destroy") {
		respond(w, http.StatusNotFound, map[string]string{"error": "接口不存在"})
		return true
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return true
	}
	if len(r.URL.Query()) != 0 {
		failure(w, platform.ErrInvalid)
		return true
	}
	if parts[1] == "stop" {
		var input struct {
			Revision int64 `json:"revision"`
		}
		if !decode(w, r, &input) {
			return true
		}
		job, requestErr := s.store.AdminRequestLightsailStop(r.Context(), who, parts[0], input.Revision)
		if requestErr != nil {
			failure(w, requestErr)
			return true
		}
		respond(w, http.StatusAccepted, job)
		return true
	}
	var input struct {
		Revision     int64  `json:"revision"`
		Confirmation string `json:"confirmation"`
	}
	if !decode(w, r, &input) {
		return true
	}
	job, requestErr := s.store.AdminRequestLightsailDestroy(r.Context(), who, parts[0], input.Revision, input.Confirmation)
	if requestErr != nil {
		failure(w, requestErr)
		return true
	}
	respond(w, http.StatusAccepted, job)
	return true
}

func methodNotAllowed(w http.ResponseWriter) {
	respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "方法不支持"})
}

func filterLightsailCatalog(catalog lightsail.Catalog) lightsail.Catalog {
	result := lightsail.Catalog{Region: catalog.Region, AvailabilityZones: catalog.AvailabilityZones, Blueprints: []lightsail.Blueprint{}, Bundles: []lightsail.Bundle{}}
	for _, blueprint := range catalog.Blueprints {
		if blueprint.Active && blueprint.Platform == "LINUX_UNIX" {
			result.Blueprints = append(result.Blueprints, blueprint)
		}
	}
	for _, bundle := range catalog.Bundles {
		if !bundle.Active {
			continue
		}
		for _, supported := range bundle.SupportedPlatforms {
			if supported == "LINUX_UNIX" {
				result.Bundles = append(result.Bundles, bundle)
				break
			}
		}
	}
	return result
}

func selectionInCatalog(selection platform.LightsailCatalogSelection, catalog lightsail.Catalog) bool {
	zoneOK, blueprintOK, bundleOK := false, false, false
	for _, zone := range catalog.AvailabilityZones {
		zoneOK = zoneOK || zone.Name == selection.AvailabilityZone && (zone.State == "" || zone.State == "available")
	}
	for _, blueprint := range catalog.Blueprints {
		blueprintOK = blueprintOK || blueprint.ID == selection.BlueprintID
	}
	for _, bundle := range catalog.Bundles {
		bundleOK = bundleOK || bundle.ID == selection.BundleID
	}
	return catalog.Region == selection.Region && zoneOK && blueprintOK && bundleOK
}
