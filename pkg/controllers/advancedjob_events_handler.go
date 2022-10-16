package controllers

import (
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

type podEventHandler struct {
	client client.Client
}

func (p *podEventHandler) Create(evt event.CreateEvent, q workqueue.RateLimitingInterface) {
}

func (p *podEventHandler) Update(evt event.UpdateEvent, q workqueue.RateLimitingInterface) {

}
func (p *podEventHandler) Delete(evt event.DeleteEvent, q workqueue.RateLimitingInterface) {

}
func (p *podEventHandler) Generic(evt event.GenericEvent, q workqueue.RateLimitingInterface) {

}

type nodeEventHandler struct {
	client client.Client
}

func (n *nodeEventHandler) Create(evt event.CreateEvent, q workqueue.RateLimitingInterface) {
}

func (n *nodeEventHandler) Update(evt event.UpdateEvent, q workqueue.RateLimitingInterface) {

}
func (n *nodeEventHandler) Delete(evt event.DeleteEvent, q workqueue.RateLimitingInterface) {

}
func (n *nodeEventHandler) Generic(evt event.GenericEvent, q workqueue.RateLimitingInterface) {

}
