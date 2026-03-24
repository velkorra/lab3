pipeline {
    agent any

    triggers {
        pollSCM('* * * * *')
    }

    environment {
        VAULT_URL = 'http://vault:8200'
        DOCKER_HOST = 'tcp://docker-dind:2376'
        REGISTRY_URL = 'registry:443'
        IMAGE_NAME = "${REGISTRY_URL}/my-quarkus-app:latest"
    }

    stages {

        stage('Auth & Issue Certs') {
            steps {
                withCredentials([
                    string(credentialsId: 'vault-role-id', variable: 'ROLE_ID'),
                    string(credentialsId: 'vault-secret-id', variable: 'SECRET_ID')
                ]) {
                    sh '''#!/bin/bash
                        set -e 
                        set +x
                        
                        echo "=> Авторизация в Vault через AppRole..."
                        TOKEN_JSON=$(curl -s -X POST -d '{"role_id":"'$ROLE_ID'","secret_id":"'$SECRET_ID'"}' $VAULT_URL/v1/auth/approle/login)
                        VAULT_TOKEN=$(echo $TOKEN_JSON | grep -o '"client_token":"[^"]*' | cut -d'"' -f4)
                        
                        if [ -z "$VAULT_TOKEN" ]; then
                            echo "ОШИБКА: Не удалось получить токен. Проверь RoleID и SecretID."
                            exit 1
                        fi

                        echo "=> Выпуск mTLS сертификата для Jenkins..."
                        curl -s -H "X-Vault-Token: $VAULT_TOKEN" -X POST -d '{"common_name":"jenkins"}' $VAULT_URL/v1/pki/issue/cicd-role > cert.json
                        
                        grep -o '"issuing_ca":"[^"]*' cert.json | cut -d'"' -f4 | perl -pe 's/\\\\n/\\n/g' > ca.crt
                        grep -o '"certificate":"[^"]*' cert.json | cut -d'"' -f4 | perl -pe 's/\\\\n/\\n/g' > client.crt
                        grep -o '"private_key":"[^"]*' cert.json | cut -d'"' -f4 | perl -pe 's/\\\\n/\\n/g' > client.key
                        
                        echo "=> Получение учетных данных writer из Vault..."
                        CREDS_JSON=$(curl -s -H "X-Vault-Token: $VAULT_TOKEN" $VAULT_URL/v1/secret/data/registry/writer)
                        REG_USER=$(echo $CREDS_JSON | grep -o '"username":"[^"]*' | head -1 | cut -d'"' -f4)
                        REG_PASS=$(echo $CREDS_JSON | grep -o '"password":"[^"]*' | head -1 | cut -d'"' -f4)
                        
                        echo -n "$REG_USER" > reg_user
                        echo -n "$REG_PASS" > reg_pass
                    '''
                }
            }
        }

        stage('Build & Push Docker Image') {
            steps {
                sh '''#!/bin/bash
                    set -e
                    set +x
                    
                    REG_USER=$(cat reg_user)
                    REG_PASS=$(cat reg_pass)
                    
                    echo "=> Логинимся в Registry ($REGISTRY_URL) под юзером $REG_USER..."
                    echo "$REG_PASS" | docker --tlsverify --tlscacert=ca.crt --tlscert=client.crt --tlskey=client.key login $REGISTRY_URL -u "$REG_USER" --password-stdin
                    
                    echo "=> Пытаемся стянуть старый образ для кэша..."
                    docker --tlsverify --tlscacert=ca.crt --tlscert=client.crt --tlskey=client.key pull $IMAGE_NAME || true

                    echo "=> Сборка образа (с кэшированием)..."
                    docker --tlsverify --tlscacert=ca.crt --tlscert=client.crt --tlskey=client.key build --cache-from $IMAGE_NAME -t $IMAGE_NAME .
                    
                    echo "=> Пуш образа в приватный Registry..."
                    docker --tlsverify --tlscacert=ca.crt --tlscert=client.crt --tlskey=client.key push $IMAGE_NAME
                '''
            }
        }
    }

    post {
        always {
            echo "=> Зачистка секретов из воркспейса..."
            sh 'rm -f cert.json ca.crt client.crt client.key reg_user reg_pass'
        }
    }
}