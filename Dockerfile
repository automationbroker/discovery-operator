FROM centos:7
MAINTAINER Fabian von Feilitzsch

ENV USER_NAME=do \
    USER_UID=1001 \
    BASE_DIR=/opt/do
ENV HOME=${BASE_DIR}

COPY discovery-operator /usr/bin/discovery-operator
COPY files/entrypoint.sh /usr/bin/entrypoint.sh
COPY files/default_config.yaml /etc/discovery-operator/config.yaml
COPY files/incluster.config ${HOME}/.kube/config

RUN useradd -u ${USER_UID} -r -g 0 -M -d ${BASE_DIR} -b ${BASE_DIR} -s /sbin/nologin -c "discover-operator user" ${USER_NAME} \
  && chmod +777 /usr/bin/{discovery-operator,entrypoint.sh} \
  && chown -R ${USER_NAME} ${HOME}/.kube/config

ENTRYPOINT ["/usr/bin/entrypoint.sh"]
