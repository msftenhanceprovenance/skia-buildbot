FROM gcr.io/skia-public/basealpine:3.8

COPY . /

USER skia

ENTRYPOINT ["/usr/local/bin/shaders"]
CMD ["--resources_dir=/usr/local/share/shaders"]
