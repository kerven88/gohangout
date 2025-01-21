package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	_ "time/tzdata"

	_ "go.uber.org/automaxprocs"

	"github.com/childe/gohangout/input"
	"github.com/childe/gohangout/internal/config"
	"github.com/childe/gohangout/internal/signal"
	"github.com/childe/gohangout/topology"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/klog/v2"
)

var (
	version   string
	buildTime string
)

var options = &struct {
	config     string
	autoReload bool // 配置文件更新自动重启
	pprof      bool
	pprofAddr  string
	cpuprofile string
	memprofile string
	version    bool

	prometheus string

	exitWhenNil bool
}{}

var (
	worker = flag.Int("worker", 1, "worker thread count")
)

type gohangoutInputs []*input.InputBox

var inputs gohangoutInputs

var (
	ctx    context.Context
	cancel context.CancelFunc
)

// start all workers in all inputboxes, and wait until stop is called (stop will shutdown all inputboxes)
func (inputs gohangoutInputs) start() {
	boxes := ([]*input.InputBox)(inputs)
	var wg sync.WaitGroup
	wg.Add(len(boxes))

	for i := range boxes {
		go func(i int) {
			defer wg.Done()
			boxes[i].Beat(*worker)
		}(i)
	}

	wg.Wait()
}

func (inputs gohangoutInputs) stop() {
	boxes := ([]*input.InputBox)(inputs)
	for _, box := range boxes {
		box.Shutdown()
	}
}

func buildPluginLink(config map[string]interface{}) (boxes []*input.InputBox, err error) {
	boxes = make([]*input.InputBox, 0)

	for inputIdx, inputI := range config["inputs"].([]interface{}) {
		var inputPlugin topology.Input

		i := inputI.(map[interface{}]interface{})
		klog.Infof("input[%d] %v", inputIdx+1, i)

		// len(i) is 1
		for inputTypeI, inputConfigI := range i {
			inputType := inputTypeI.(string)
			inputConfig := inputConfigI.(map[interface{}]interface{})

			inputPlugin = input.GetInput(inputType, inputConfig)
			if inputPlugin == nil {
				err = fmt.Errorf("invalid input plugin")
				return
			}

			box := input.NewInputBox(inputPlugin, inputConfig, config, exit)
			if box == nil {
				err = fmt.Errorf("new input box fail")
				return
			}
			box.SetShutdownWhenNil(options.exitWhenNil)
			boxes = append(boxes, box)
		}
	}

	return
}

// reload config file. stop inputs and start new inputs
func reload() {
	gohangoutConfig, err := config.ParseConfig(options.config)
	if err != nil {
		klog.Errorf("could not parse config, ignore reload: %v", err)
		return
	}
	klog.Info("stop old inputs")
	inputs.stop()

	boxes, err := buildPluginLink(gohangoutConfig)
	if err != nil {
		klog.Errorf("build plugin link error, ignore reload: %v", err)
		return
	}
	inputs = gohangoutInputs(boxes)
	klog.Info("start new inputs")
	go inputs.start()
}

func _main() {
	gohangoutConfig, err := config.ParseConfig(options.config)
	if err != nil {
		klog.Fatalf("could not parse config: %v", err)
	}

	boxes, err := buildPluginLink(gohangoutConfig)
	if err != nil {
		klog.Fatalf("build plugin link error: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	inputs = gohangoutInputs(boxes)
	go inputs.start()

	if options.autoReload {
		if err := config.WatchConfig(options.config, reload); err != nil {
			klog.Fatalf("watch config fail: %s", err)
		}
	}

	go signal.ListenSignal(exit, reload)

	<-ctx.Done()
	inputs.stop()
}

func main() {
	flag.StringVar(&options.config, "config", options.config, "path to configuration file or directory")
	flag.BoolVar(&options.autoReload, "reload", options.autoReload, "if auto reload while config file changed")

	flag.BoolVar(&options.pprof, "pprof", false, "pprof or not")
	flag.StringVar(&options.pprofAddr, "pprof-address", "127.0.0.1:8899", "default: 127.0.0.1:8899")
	flag.StringVar(&options.cpuprofile, "cpuprofile", "", "write cpu profile to `file`")
	flag.StringVar(&options.memprofile, "memprofile", "", "write mem profile to `file`")

	flag.BoolVar(&options.version, "version", false, "print version and exit")

	flag.StringVar(&options.prometheus, "prometheus", "", "address to expose prometheus metrics")

	flag.BoolVar(&options.exitWhenNil, "exit-when-nil", false, "triger gohangout to exit when receive a nil event")

	klog.InitFlags(nil)
	flag.Parse()

	if options.version {
		fmt.Printf("gohangout: %s compiled at %s with %v on %v/%v\n", version, buildTime, runtime.Version(), runtime.GOOS, runtime.GOARCH)
		return
	}

	klog.Infof("gohangout version: %s", version)
	defer klog.Flush()

	if options.prometheus != "" {
		go func() {
			http.Handle("/metrics", promhttp.Handler())
			http.ListenAndServe(options.prometheus, nil)
		}()
	}

	if options.pprof {
		go func() {
			http.ListenAndServe(options.pprofAddr, nil)
		}()
	}
	if options.cpuprofile != "" {
		f, err := os.Create(options.cpuprofile)
		if err != nil {
			klog.Fatalf("could not create CPU profile: %s", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			klog.Fatalf("could not start CPU profile: %s", err)
		}
		defer pprof.StopCPUProfile()
	}

	if options.memprofile != "" {
		defer func() {
			f, err := os.Create(options.memprofile)
			if err != nil {
				klog.Fatalf("could not create memory profile: %s", err)
			}
			defer f.Close()
			runtime.GC() // get up-to-date statistics
			if err := pprof.WriteHeapProfile(f); err != nil {
				klog.Fatalf("could not write memory profile: %s", err)
			}
		}()
	}

	_main()

}

func exit() {
	cancel()
}
