## Quarkus Rate Limiting API

Quarkus-приложение на Gradle с ограничением количества обрабатываемых запросов через rate limiting.

### Структура проекта

- **Код**: `src/main/java/org/example/`
  - `WorkResource.java` - REST-эндпоинты с rate limiting
- **Конфигурация**: `src/main/resources/application.properties`
- **Сборка**: Gradle (`build.gradle`, `settings.gradle`)

### Конфигурация rate limiting

В `application.properties` настраивается лимит запросов:

```properties
app.api.limit=${APP_API_LIMIT:130}
app.api.timeout=${APP_API_TIMEOUT:4000}
```

- `app.api.limit` - максимальное количество одновременных запросов (по умолчанию 130)
- `app.api.timeout` - задержка обработки запроса в миллисекундах (по умолчанию 4000)

При превышении лимита возвращается HTTP 429 (Too Many Requests).

### Эндпоинты

- `GET /work` - основная точка для нагрузочного теста. Обрабатывает запрос с задержкой и возвращает `OK` при успехе, либо `429` при превышении лимита.
- `GET /work/status` - возвращает текущее количество активных запросов.

### Swagger UI

Приложение включает поддержку Swagger/OpenAPI для документирования и тестирования API.

После запуска приложения доступны:

- **Swagger UI**: `http://localhost:8080/q/swagger-ui` - интерактивная документация и тестирование API
- **OpenAPI спецификация (JSON)**: `http://localhost:8080/q/openapi` - OpenAPI спецификация в формате JSON

### Запуск сервиса

Из корня репозитория:

```bash
./gradlew quarkusDev
```

Сервис поднимется на `http://localhost:8080` (порт настраивается в `application.properties`).

### Сборка и запуск в production режиме

```bash
# Сборка JAR
./gradlew build

# Запуск собранного приложения
java -jar build/quarkus-app/quarkus-run.jar

#Сборка в нативном режиме
./gradlew --no-daemon --build-cache build -x test -x spotlessJavaApply -x spotlessJava -Dquarkus.native.enabled=true -Dquarkus.native.remote-container-build=false -Dquarkus.package.jar.enabled=false
```
