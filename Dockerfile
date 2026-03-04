# СТАДИЯ 1: Сборка (GraalVM 21 на базе Oracle Linux)
FROM ghcr.io/graalvm/native-image-community:21 AS build
WORKDIR /code

# Копируем только файлы сборки
COPY gradlew /code/
COPY gradle /code/gradle
COPY build.gradle settings.gradle /code/

# Запускаем Gradle напрямую через Java (это обходит ошибку 'xargs is not available')
RUN java -cp gradle/wrapper/gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain dependencies --no-daemon

# Копируем исходники
COPY src /code/src

# Собираем native бинарник (также напрямую через Java)
RUN java -cp gradle/wrapper/gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain build -Dquarkus.package.type=native -x test --no-daemon

# СТАДИЯ 2: Минимальный Runtime
FROM debian:bookworm-slim
WORKDIR /work/

# Устанавливаем zlib (нужна для работы скомпилированного бинарника)
RUN apt-get update && apt-get install -y libz1 && rm -rf /var/lib/apt/lists/*

COPY --from=build /code/build/*-runner /work/application
RUN chmod 775 /work/application

# Твой Вариант 6
ENV APP_API_LIMIT=400
ENV APP_API_TIMEOUT=7000

EXPOSE 8080
CMD ["./application", "-Dquarkus.http.host=0.0.0.0"]