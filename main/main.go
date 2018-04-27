package main

import (
	"github.com/kataras/golog"
	"github.com/ant0ine/go-json-rest/rest"
	"net/http"
	"time"
	"github.com/r-medina/go-uber"
	"github.com/lyft/lyft-go-sdk/lyft"
	"strconv"
	"golang.org/x/oauth2/clientcredentials"
	"fmt"
	"vector/keys"
	"log"
	"context"
	"googlemaps.github.io/maps"
	"crypto/sha256"
)

var lyftClientConfig = clientcredentials.Config{
	ClientID:     keys.LYFT_CLIENT,
	ClientSecret: keys.LYFT_SECRET,
	TokenURL:     "https://api.lyft.com/oauth/token",
	Scopes:       []string{"public"},
}
var uberClient *uber.Client
var lyftClient *lyft.APIClient
var mapsClient *maps.Client
var responseMap map[string]Comp

func main() {
	golog.SetLevel("info")
	golog.Info("Starting vector v0.1")

	golog.Info("Initializing JSON API")
	api := rest.NewApi()
	api.Use(rest.DefaultDevStack...)
	golog.Info("Creating API paths")
	router, err := rest.MakeRouter(
		rest.Post("/comp", GetComp),
	)
	if err != nil {
		golog.Fatal(err)
	}
	golog.Info("Connecting API to router")
	api.SetApp(router)
	golog.Info("Making responseMap")
	responseMap = make(map[string]Comp)
	golog.Info("Starting HTTP server")
	log.Fatal(http.ListenAndServe(":8888", api.MakeHandler()))
}

type CompArgs struct {
	PickupLat string `json:"pickupLat"`
	PickupLon string `json:"pickupLon"`
	DestLat   string `json:"destLat"`
	DestLon   string `json:"destLon"`
}

type Comp struct {
	Timestamp int64
	PriceUber string
	TimeUber  string
	PriceLyft string
	TimeLyft  string
	TimeMBTA  string
}

func GetComp(w rest.ResponseWriter, r *rest.Request) {
	var uberPriceRange string
	var uberTime string
	var lyftPriceRange string
	var lyftTime string
	var mbtaTime string

	ca := CompArgs{}
	err := r.DecodeJsonPayload(&ca)
	if err != nil {
		golog.Error("Could not decode payload for comp")
		golog.Error(err)
	}
	pickupLat := ca.PickupLat
	pickupLon := ca.PickupLon
	destLat := ca.DestLat
	destLon := ca.DestLon

	//TODO: caching mechanism derivation for hashkey
	storeToResponseMap := false
	hasher := sha256.New()
	hasher.Write([]byte(pickupLat))
	hasher.Write([]byte(pickupLon))
	hasher.Write([]byte(destLat))
	hasher.Write([]byte(destLon))
	hashKey := fmt.Sprintf("%x", hasher.Sum(nil))
	if response, ok := responseMap[hashKey]; ok {
		golog.Info("Got cached request! Checking timestamp")
		cacheTime := time.Unix(response.Timestamp, 0)
		secondsSince := time.Since(cacheTime).Seconds()
		if secondsSince <= 30 {
			golog.Info("Sending cached request to client")
			w.WriteJson(&response)
			return
		} else {
			golog.Infof("Cache is too old (%v sec ago)!", secondsSince)
			storeToResponseMap = true
		}
	} else {
		golog.Info("Missed cache!")
		storeToResponseMap = true
	}

	pickupStr := pickupLat + ", " + pickupLon
	destStr := destLat + ", " + destLon

	golog.Debug("Starting float parsing")
	floatPickupLat, err := strconv.ParseFloat(pickupLat, 64)
	if err != nil {
		handleApiErr(err, w, r)
		return
	}
	floatPickupLon, err := strconv.ParseFloat(pickupLon, 64)
	if err != nil {
		handleApiErr(err, w, r)
		return
	}
	floatDestLat, err := strconv.ParseFloat(destLat, 64)
	if err != nil {
		handleApiErr(err, w, r)
		return
	}
	floatDestLon, err := strconv.ParseFloat(destLon, 64)
	if err != nil {
		handleApiErr(err, w, r)
		return
	}
	golog.Debug("Float parsing complete!")

	//TODO: actual logic for pricing
	golog.Debugf("Handling comp from (%s, %s) to (%s, %s)", pickupLat, pickupLon, destLat, destLon)

	// Uber API
	golog.Debug("Creating uber client")
	if uberClient == nil {
		uberClient = uber.NewClient(keys.UBER_KEY)
	}
	golog.Debug("Calling uber API -> price")
	prices, err := uberClient.GetPrices(floatPickupLat, floatPickupLon, floatDestLat, floatDestLon)
	if err != nil {
		handleApiErr(err, w, r)
		return
	}
	if len(prices) > 0 {
		golog.Info("Processing uber cost data")
		price := prices[0]
		lowStr := strconv.Itoa(price.LowEstimate)
		highStr := strconv.Itoa(price.HighEstimate)
		uberPriceRange = "$" + lowStr + " - $" + highStr
		golog.Debug("Calling uber API -> time")
		uberTimes, err := uberClient.GetTimes(floatPickupLat, floatPickupLon, "", "")
		if err != nil {
			handleApiErr(err, w, r)
			return
		}
		if len(uberTimes) > 0 {
			golog.Info("Processing uber time data")
			uberTime = strconv.Itoa(uberTimes[0].Estimate / 60) + " mins"
		} else {
			handleApiErr(err, w, r)
			return
		}
	}

	// Lyft API
	if lyftClient == nil {
		httpClient := lyftClientConfig.Client(context.Background())
		lyftClient = lyft.NewAPIClient(httpClient, "vector")
	}
	lyftOpts := map[string]interface{}{
		"endLat": floatDestLat,
		"endLng": floatDestLon,
		"rideType": string(lyft.RideTypeLyft),
	}
	golog.Debugf("Calling lyft API -> price (%v)", lyftOpts)
	costResult, response, err := lyftClient.PublicApi.GetCost(floatPickupLat, floatPickupLon, lyftOpts)
	if err != nil {
		var responseBytes []byte
		response.Body.Read(responseBytes)
		golog.Errorf("Lyft price API error: %s", responseBytes)
		handleApiErr(err, w, r)
		return
	}
	if len(costResult.CostEstimates) > 0 {
		golog.Info("Processing lyft cost data")
		costMinFloat := float64(costResult.CostEstimates[0].EstimatedCostCentsMin) / 100.0
		costMaxFloat := float64(costResult.CostEstimates[0].EstimatedCostCentsMax) / 100.0
		golog.Debugf("Lyft costMin: %v", costResult.CostEstimates[0].EstimatedCostCentsMin)
		golog.Debugf("Lyft costMax: %v", costResult.CostEstimates[0].EstimatedCostCentsMax)
		costMin := strconv.FormatFloat(costMinFloat, 'f', 2, 64)
		costMax := strconv.FormatFloat(costMaxFloat, 'f', 2, 64)
		if costMin == costMax {
			lyftPriceRange = "$" + costMin
		} else {
			lyftPriceRange = "$" + costMin + " - $" + costMax
		}
	}
	golog.Debug("Calling lyft API -> time")
	result, _, err := lyftClient.PublicApi.GetETA(floatPickupLat, floatPickupLon, nil)
	if err != nil {
		handleApiErr(err, w, r)
		return
	}
	if len(result.EtaEstimates) > 0 {
		golog.Info("Processing lyft time data")
		etaSeconds := result.EtaEstimates[0].EtaSeconds
		etaMins := etaSeconds / 60.0
		lyftTime = fmt.Sprintf("%d mins", etaMins)
	}

	// Transit API
	if mapsClient == nil {
		mapsClient, err = maps.NewClient(maps.WithAPIKey(keys.GOOG_DIRECT))
		if err != nil {
			handleApiErr(err, w, r)
			return
		}
	}
	directionsReq := &maps.DirectionsRequest{
		Origin:      pickupStr,
		Destination: destStr,
		Mode:        maps.TravelModeTransit,
	}
	golog.Info("Calling google API -> transit time")
	route, _, err := mapsClient.Directions(context.Background(), directionsReq)
	if err != nil {
		handleApiErr(err, w, r)
		return
	}
	if len(route) > 0 {
		golog.Debugf("Found %d transit routes", len(route))
		chosenRoute := route[0]
		if len(chosenRoute.Legs) > 0 {
			golog.Debugf("Found %d route legs", len(chosenRoute.Legs))
			travelTime := time.Until(chosenRoute.Legs[len(chosenRoute.Legs) - 1].ArrivalTime).Seconds()
			golog.Debugf("Travel last leg time of arrival: %v", travelTime)
			intTravelTime := int(travelTime / 60)
			mbtaTime = fmt.Sprintf("%d mins (trip)", intTravelTime)
			golog.Info("Processing google transit time data")
		}
	}

	//TODO: caching mechanism based on timestamp and coordinates
	newComp := Comp{
		time.Now().Unix(),
		uberPriceRange,
		uberTime,
		lyftPriceRange,
		lyftTime,
		mbtaTime,
	}
	if storeToResponseMap {
		golog.Info("Stored response in cache!")
		responseMap[hashKey] = newComp
	}
	w.WriteJson(&newComp)
}

func handleApiErr(err error, w rest.ResponseWriter, r *rest.Request) {
	golog.Error(err)
	rest.NotFound(w, r)
	return
}
