FROM ghcr.io/graalvm/native-image-community:21 AS build
WORKDIR /code

COPY gradlew /code/
COPY gradle /code/gradle
COPY build.gradle settings.gradle /code/

RUN java -cp gradle/wrapper/gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain dependencies --no-daemon

COPY src /code/src

RUN java -cp gradle/wrapper/gradle-wrapper.jar org.gradle.wrapper.GradleWrapperMain build -Dquarkus.package.type=native -x test --no-daemon

FROM debian:bookworm-slim
WORKDIR /work/

RUN apt-get update && apt-get install -y libz1 && rm -rf /var/lib/apt/lists/*

COPY --from=build /code/build/*-runner /work/application
RUN chmod 775 /work/application

ENV APP_API_LIMIT=480
ENV APP_API_TIMEOUT=3500

EXPOSE 8080
CMD ["./application", "-Dquarkus.http.host=0.0.0.0"]