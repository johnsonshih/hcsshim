package main

import (
	"context"
	"encoding/json"
	"flag"
	"io/ioutil"
	"log"
	"os"
	"time"

	"github.com/Microsoft/hcsshim/internal/hcs"
	hcsschema "github.com/Microsoft/hcsshim/internal/schema2"
)

var (
	vmNameFlag       = flag.String("vmname", "", "vm name")
	jsonFileNameFlag = flag.String("json", "", "modify request json path")
)

func main() {
	flag.Parse()
	if flag.NArg() != 0 || len(*vmNameFlag) == 0 || len(*jsonFileNameFlag) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	if err := modifyComputeSystem(*vmNameFlag, *jsonFileNameFlag); err != nil {
		os.Exit(1)
	}
}

func modifyComputeSystem(vmName string, jsonFileName string) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()

	requestContent, err := ioutil.ReadFile(jsonFileName)
	if err != nil {
		log.Printf("ReadFile json %s failed, err = %v", jsonFileName, err)
		return
	}

	var request hcsschema.ModifySettingRequest

	err = json.Unmarshal(requestContent, &request)
	if err != nil {
		log.Printf("Unmarshal json failed, err = %v", err)
		return
	}

	system, err := hcs.OpenComputeSystem(ctx, vmName)
	if err != nil {
		log.Printf("OpenComputeSystem failed, err = %v", err)
		return
	}
	defer system.Close()

	if err = system.Modify(ctx, request); err != nil {
		log.Printf("ModifyComputeSystem failed, err = %v", err)
		return
	}

	log.Printf("ModifyComputeSystem Succeeded")
	return
}
