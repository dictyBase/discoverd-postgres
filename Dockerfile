FROM cybersiddhu/centos
MAINTAINER 'Siddhartha Basu<siddhartha-basu@northwestern.edu>'

RUN mkdir /config && echo 'NETWORKING=yes' > /etc/sysconfig/network
ADD config /config
RUN rpm -ivh http://yum.postgresql.org/9.2/redhat/rhel-6-x86_64/pgdg-centos92-9.2-6.noarch.rpm &&\
    yum -y install postgresql92-server postgresql92-contrib rsync 

ENV LANG en_US.UTF-8
ENV LC_ALL en_US.UTF-8 

ADD bin/discoverd-postgres /usr/local/bin/discoverd-postgres
ADD start.sh /usr/local/bin/start-discoverd-postgres

CMD /usr/local/bin/start-discoverd-postgres
