package events

const (
	eventnKey       = "eventn_ctx"
	eventIdKey      = "event_id"
	collectionIdKey = "collection_id"
)

func EnrichWithEventId(object map[string]interface{}, eventId string) {
	eventnObject, ok := object[eventnKey]
	if !ok {
		eventnObject = map[string]interface{}{eventIdKey: eventId}
		object[eventnKey] = eventnObject
	} else {
		if eventn, ok := eventnObject.(map[string]interface{}); ok {
			if _, ok := eventn[eventIdKey]; !ok {
				eventn[eventIdKey] = eventId
			}
		} else {
			object[eventnKey+"_"+eventIdKey] = eventId
		}
	}
}

func EnrichWithCollection(object map[string]interface{}, collection string) {
	eventnObject, ok := object[eventnKey]
	if !ok {
		eventnObject = map[string]interface{}{collectionIdKey: collection}
		object[eventnKey] = eventnObject
	} else {
		if eventn, ok := eventnObject.(map[string]interface{}); ok {
			if _, ok := eventn[collectionIdKey]; !ok {
				eventn[collectionIdKey] = collection
			}
		} else {
			object[eventnKey+"_"+collectionIdKey] = collection
		}
	}
}
