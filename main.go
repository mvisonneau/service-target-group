package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"reflect"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/elbv2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/runtime/signals"
)

func main() {
	reconciler := &endpointReconciler{
		managedResources: make(map[string]map[string]*elbv2.TargetDescription, 0),
	}
	manager, err := builder.SimpleController().
		ForType(&corev1.Service{}).
		Owns(&corev1.Endpoints{}).
		Build(reconciler)

	if err != nil {
		log.Println("Unable to build controller:", err)
		os.Exit(1)
	}

	reconciler.client = manager.GetClient()

	if err := manager.Start(signals.SetupSignalHandler()); err != nil {
		log.Println("Unable to run controller:", err)
		os.Exit(1)
	}

}

type endpointReconciler struct {
	client           client.Client
	managedResources map[string]map[string]*elbv2.TargetDescription
}

func (r *endpointReconciler) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	if request.NamespacedName.Namespace != "default" { // TODO
		return reconcile.Result{}, nil
	}

	rss := &corev1.Service{}
	err := r.client.Get(context.TODO(), request.NamespacedName, rss)
	if errors.IsNotFound(err) {
		delete(r.managedResources, request.NamespacedName.String())
		// TODO deregister everything?
		return reconcile.Result{}, nil
	}

	targetGroupARN := rss.Annotations["stg.monder.cc/target-group"]
	if targetGroupARN == "" { // Skip services that we do not need to register
		return reconcile.Result{}, nil
	}

	rse := &corev1.Endpoints{}
	err = r.client.Get(context.TODO(), request.NamespacedName, rse)
	if errors.IsNotFound(err) {
		delete(r.managedResources, request.NamespacedName.String())
		// TODO deregister everything?
		return reconcile.Result{}, nil
	}

	newState := make(map[string]*elbv2.TargetDescription, 0)

	for _, s := range rse.Subsets {
		for _, p := range s.Ports {
			for _, a := range s.Addresses {
				newState[fmt.Sprintf("%s:%d", a.IP, p.Port)] = &elbv2.TargetDescription{
					Id:   aws.String(a.IP),
					Port: aws.Int64(int64(p.Port)),
				}
			}
		}
	}

	if reflect.DeepEqual(newState, r.managedResources[request.NamespacedName.String()]) {
		return reconcile.Result{}, nil
	}

	targetsToDeregister := make([]*elbv2.TargetDescription, 0)
	targetsToRegister := make([]*elbv2.TargetDescription, 0)

	svc := elbv2.New(session.New())
	input := &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(targetGroupARN),
	}

	result, err := svc.DescribeTargetHealth(input)
	if err != nil {
		fmt.Println(err.Error())
		return reconcile.Result{}, nil
	}

	for _, th := range result.TargetHealthDescriptions {
		_, keep := newState[fmt.Sprintf("%s:%d", *th.Target.Id, *th.Target.Port)]
		if !keep {
			targetsToDeregister = append(targetsToDeregister, th.Target)
		}
	}

	for _, td := range newState {
		found := false
		for _, th := range result.TargetHealthDescriptions {
			if *th.Target.Id == *td.Id && *th.Target.Port == *td.Port && *th.TargetHealth.State != elbv2.TargetHealthStateEnumDraining {
				found = true
				break
			}
		}
		if !found {
			targetsToRegister = append(targetsToRegister, td)
		}
	}

	fmt.Println("dereg:")
	fmt.Println(targetsToDeregister)
	fmt.Println("reg:")
	fmt.Println(targetsToRegister)

	// Register
	if len(targetsToRegister) > 0 {
		input2 := &elbv2.RegisterTargetsInput{
			TargetGroupArn: aws.String(targetGroupARN),
			Targets:        targetsToRegister,
		}
		_, err = svc.RegisterTargets(input2)
		fmt.Println(err)
	}

	// Deregister
	if len(targetsToDeregister) > 0 {
		input3 := &elbv2.DeregisterTargetsInput{
			TargetGroupArn: aws.String(targetGroupARN),
			Targets:        targetsToDeregister,
		}
		_, err = svc.DeregisterTargets(input3)
		fmt.Println(err)
	}

	fmt.Println("---")
	r.managedResources[request.NamespacedName.String()] = newState
	return reconcile.Result{}, nil
}
