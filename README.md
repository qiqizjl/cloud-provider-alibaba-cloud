# 关于这个仓库
这个repo由[kubernetes/cloud-provider-alibaba-cloud](https://github.com/kubernetes/cloud-provider-alibaba-cloud) fork
出来

## 增加特性
本仓库增加以下特性
- 支持Local模式下SLB挂载到master节点
- 解决使用ECI虚拟节点下报错的问题

# Kubernetes Cloud Controller Manager for Alibaba Cloud

Thank you for visiting the cloud-provider-alibaba-cloud repository!


`cloud-provider-alibaba-cloud` is the external Kubernetes cloud controller manager implementation for Alibaba Cloud. Running `cloud-provider-alibaba-cloud` allows you build your kubernetes clusters leverage on many cloud services on Alibaba Cloud. You can read more about Kubernetes cloud controller manager [here](https://kubernetes.io/docs/tasks/administer-cluster/running-cloud-controller/).

## Development

Test project with command ```make test``` and Build an image with command ```make image```

## QuickStart

- [Getting-started](docs/getting-started.md)
- [Usage Guide](docs/usage.md)


## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack channel](https://kubernetes.slack.com/messages/sig-cloud-provider)
- [Mailing list](https://groups.google.com/forum/#!forum/kubernetes-sig-cloud-provider)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

## Testing
See more info in page [Test](https://github.com/kubernetes/cloud-provider-alibaba-cloud/tree/master/docs/testing.md)