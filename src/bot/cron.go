package bot

import (
	"context"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/fatih/structs"
	"google.golang.org/api/iterator"
)

// Cron makes matches for pairing, and messages those people to notify them of their match
// it runs once per day at 8am (it's triggered with app engine's Cron service)
func Cron(w http.ResponseWriter, r *http.Request) {
	// Check that the request is originating from within app engine
	// https://cloud.google.com/appengine/docs/flexible/go/scheduling-jobs-with-cron-yaml#validating_cron_requests
	if r.Header.Get("X-Appengine-Cron") != "true" {
		http.NotFound(w, r)
		return
	}

	// setting up database connection
	ctx := context.Background()
	var err error
	client, err = firestore.NewClient(ctx, "mock-interview-bot-307121")
	defer client.Close()

	if err != nil {
		log.Panic(err)
	}

	messageSolo(client, ctx)
	messagePairs(client, ctx)
}

func messageSolo(client *firestore.Client, ctx context.Context) {

	var recursersList []Recurser
	var skippersList []Recurser

	// this gets the time from system time, which is UTC
	// on app engine (and most other places). This works
	// fine for us in NYC, but might not if pairing bot
	// were ever running in another time zone
	today := strings.ToLower(time.Now().Weekday().String())[:3]

	// ok this is how we have to get all the recursers. it's weird.
	// this query returns an iterator, and then we have to use firestore
	// magic to iterate across the results of the query and store them
	// into our 'recursersList' variable which is a slice of map[string]interface{}
	iter := client.Collection("recursers").
		Where("isSkippingTomorrow", "==", false).
		Where("soloDays", "array-contains", today).
		Documents(ctx)

	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Panic(err)
		}

		var recurser Recurser
		if err = doc.DataTo(&recurser); err != nil {
			log.Fatal(err)
		}
		recursersList = append(recursersList, recurser)
	}

	// get everyone who was set to skip today and set them back to isSkippingTomorrow = false
	iter = client.Collection("recursers").Where("isSkippingTomorrow", "==", true).Documents(ctx)
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Panic(err)
		}

		var recurser Recurser
		if err = doc.DataTo(&recurser); err != nil {
			log.Fatal(err)
		}
		skippersList = append(skippersList, recurser)
	}

	for i := range skippersList {
		skippersList[i].IsSkippingTomorrow = false
		_, err := client.Collection("recursers").Doc(skippersList[i].Id).Set(ctx, structs.Map(skippersList[i]), firestore.MergeAll)
		if err != nil {
			log.Println(err)
		}
	}

	// shuffle our recursers. This will not error if the list is empty
	// recursersList = shuffle(recursersList)

	// if for some reason there's no matches today, we're done
	if len(recursersList) == 0 {
		log.Println("No one signed up for a daily question")
		return
	}

	// message the peeps!
	doc, err := client.Collection("apiauth").Doc("key").Get(ctx)
	if err != nil {
		log.Panic(err)
	}

	apikey := doc.Data()
	botPassword := apikey["value"].(string)
	zulipClient := &http.Client{}

	for i := range recursersList {
		messageRequest := url.Values{}
		messageRequest.Add("type", "private")
		messageRequest.Add("to", recursersList[i].Email)
		messageRequest.Add("content", botMessages.Matched)
		req, err := http.NewRequest("POST", zulipAPIURL, strings.NewReader(messageRequest.Encode()))
		req.SetBasicAuth(botEmailAddress, botPassword)
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
		resp, err := zulipClient.Do(req)
		if err != nil {
			log.Panic(err)
		}
		defer resp.Body.Close()
		respBodyText, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Println(err)
		}
		log.Println(string(respBodyText))
		log.Println("A match went out")
	}
}

func messagePairs(client *firestore.Client, ctx context.Context) {

	var recursersList []Recurser

	// ok this is how we have to get all the recursers. it's weird.
	// this query returns an iterator, and then we have to use firestore
	// magic to iterate across the results of the query and store them
	// into our 'recursersList' variable which is a slice of map[string]interface{}
	iter := client.Collection("recursers").Where("isPairingTomorrow", "==", true).Documents(ctx)
	for {
		doc, err := iter.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			log.Panic(err)
		}

		var recurser Recurser
		if err = doc.DataTo(&recurser); err != nil {
			log.Fatal(err)
		}
		recursersList = append(recursersList, recurser)
	}

	// shuffle our recursers. This will not error if the list is empty
	optimalPath := determineBestPath(recursersList)
	recursersList = optimalPath.order

	// if for some reason there's no matches today, we're done
	if len(recursersList) == 0 {
		log.Println("No one was signed up to pair today -- so there were no matches")
		return
	}

	// message the peeps!
	doc, err := client.Collection("apiauth").Doc("key").Get(ctx)
	if err != nil {
		log.Panic(err)
	}
	apikey := doc.Data()
	botPassword := apikey["value"].(string)
	zulipClient := &http.Client{}

	// if there's an odd number today, message the last person in the list
	// and tell them they don't get a match today, then knock them off the list
	if len(recursersList)%2 != 0 {
		recurser := recursersList[len(recursersList)-1]
		recursersList = recursersList[:len(recursersList)-1]
		log.Println("Someone was the odd-one-out today")
		messageRequest := url.Values{}
		messageRequest.Add("type", "private")
		messageRequest.Add("to", recurser.Email)
		messageRequest.Add("content", botMessages.OddOneOut)
		req, err := http.NewRequest("POST", zulipAPIURL, strings.NewReader(messageRequest.Encode()))
		req.SetBasicAuth(botEmailAddress, botPassword)
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
		resp, err := zulipClient.Do(req)
		if err != nil {
			log.Panic(err)
		}
		defer resp.Body.Close()
		respBodyText, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Println(err)
		}
		log.Println(string(respBodyText))
	}

	for i := 0; i < len(recursersList); i += 2 {
		messageRequest := url.Values{}
		messageRequest.Add("type", "private")
		messageRequest.Add("to", recursersList[i].Email+", "+recursersList[i+1].Email)
		messageRequest.Add("content", botMessages.Matched)
		req, err := http.NewRequest("POST", zulipAPIURL, strings.NewReader(messageRequest.Encode()))
		req.SetBasicAuth(botEmailAddress, botPassword)
		req.Header.Set("content-type", "application/x-www-form-urlencoded")
		resp, err := zulipClient.Do(req)
		if err != nil {
			log.Panic(err)
		}
		defer resp.Body.Close()
		respBodyText, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			log.Println(err)
		}
		log.Println(string(respBodyText))
		log.Println("A match went out")
	}
}

type Path struct {
	order      []Recurser
	validPairs int
}

func determineBestPath(recursers []Recurser) Path {
	bestPath := new(Path)
	stack := make([]Path, 0)
	for _, recurser := range recursers {
		stack = append(stack, Path{[]Recurser{recurser}, 0})
	}

	wg := new(sync.WaitGroup)
	bestPossibleScore := len(recursers) / 2

	for i := range stack {
		wg.Add(1)
		go func(path Path) {
			getNext(path, recursers, map[string]bool{path.order[0].Id: true}, bestPath, bestPossibleScore, wg)
			defer wg.Done()
		}(stack[i])
	}
	wg.Wait()

	return *bestPath
}

func getNext(path Path, recursers []Recurser, seen map[string]bool, bestPath *Path, bestPossibleScore int, wg *sync.WaitGroup) {
	if bestPath.validPairs == bestPossibleScore {
		return
	}

	if len(path.order)%2 == 0 {
		if isValidSoFar(path.order) {
			path.validPairs++
		}
		// 	if !isWorthExploring(path, bestPossibleScore, bestPath.validPairs) {
		// 		return
		// 	}
	}

	if len(path.order) == len(recursers) && path.validPairs > bestPath.validPairs {
		*bestPath = path
		return
	}

	for _, recurser := range recursers {
		if _, ok := seen[recurser.Id]; ok {
			continue
		}

		pathCopy := path
		pathCopy.order = make([]Recurser, len(path.order))
		copy(pathCopy.order, path.order)
		pathCopy.order = append(pathCopy.order, recurser)

		seenCopy := make(map[string]bool, 0)
		for k, v := range seen {
			seenCopy[k] = v
		}
		seenCopy[recurser.Id] = true

		wg.Add(1)
		go func() {
			getNext(pathCopy, recursers, seenCopy, bestPath, bestPossibleScore, wg)
			defer wg.Done()
		}()

	}
}

func isValidSoFar(path []Recurser) bool {
	recurserOne := path[len(path)-2]
	recurserTwo := path[len(path)-1]

	difficulties := map[string]int{
		"easy":   0,
		"medium": 1,
		"hard":   2,
	}

	return min(recurserOne.Config.PairingDifficulty, difficulties) <= difficulties[recurserTwo.Config.Experience] &&
		min(recurserTwo.Config.PairingDifficulty, difficulties) <= difficulties[recurserOne.Config.Experience]
}

// func isWorthExploring(path Path, bestPossibleScore int, bestScore int) bool {
// 	return bestPossibleScore-len(path.order)+path.validPairs >= bestScore
// }
