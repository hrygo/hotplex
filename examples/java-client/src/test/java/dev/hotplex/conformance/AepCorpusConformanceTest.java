package dev.hotplex.conformance;

import static org.junit.jupiter.api.Assertions.*;

import java.io.IOException;
import java.nio.file.*;
import java.util.*;
import java.util.stream.*;

import org.junit.jupiter.api.*;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.MethodSource;

import dev.hotplex.protocol.EventKind;
import dev.hotplex.protocol.InputAckData;

import com.fasterxml.jackson.databind.JsonNode;
import com.fasterxml.jackson.databind.ObjectMapper;

/**
 * AEP v1 cross-SDK conformance test (issue #869).
 *
 * Loads the golden corpus fixtures from pkg/aep/schema/corpus/ and validates
 * that every envelope parses correctly through the Java SDK's Jackson mapper.
 * Unknown additive kinds must not cause errors (forward compatibility).
 */
class AepCorpusConformanceTest {

    private static final ObjectMapper MAPPER = new ObjectMapper();

    // examples/java-client -> examples -> repo root -> pkg/aep/schema/corpus
    private static final Path CORPUS_DIR = Path.of("..", "..", "pkg", "aep", "schema", "corpus");

    private static List<Path> corpusFiles;

    @BeforeAll
    static void loadCorpus() throws IOException {
        corpusFiles = Files.list(CORPUS_DIR)
                .filter(p -> p.toString().endsWith(".json"))
                .sorted()
                .collect(Collectors.toList());
    }

    @Test
    @DisplayName("corpus directory exists and is non-empty")
    void corpusDirectoryExists() {
        assertTrue(Files.isDirectory(CORPUS_DIR), "Corpus dir not found: " + CORPUS_DIR.toAbsolutePath());
        assertFalse(corpusFiles.isEmpty(), "Corpus directory is empty");
    }

    static Stream<Path> corpusProvider() {
        return corpusFiles.stream();
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("corpusProvider")
    @DisplayName("corpus envelope has valid AEP structure")
    void envelopeIsValid(Path fixture) throws IOException {
        JsonNode env = MAPPER.readTree(fixture.toFile());

        assertEquals("aep/v1", env.get("version").asText(),
                fixture + ": wrong version");
        assertTrue(env.has("id") && env.get("id").isTextual(),
                fixture + ": missing id");
        assertTrue(env.has("event"), fixture + ": missing event");
        JsonNode type = env.get("event").get("type");
        assertNotNull(type, fixture + ": missing event.type");
        assertTrue(type.isTextual(), fixture + ": event.type not string");
    }

    @Test
    @DisplayName("covers all stable kinds (>= 32 non-edge-case fixtures)")
    void coversAllStableKinds() throws IOException {
        Set<String> stableKinds = new HashSet<>();
        for (Path f : corpusFiles) {
            if (f.getFileName().toString().startsWith("9")) continue;
            JsonNode env = MAPPER.readTree(f.toFile());
            stableKinds.add(env.get("event").get("type").asText());
        }
        assertTrue(stableKinds.size() >= 32,
                "Only " + stableKinds.size() + " stable kinds, expected >= 32");
    }

    @Test
    @DisplayName("unknown kind is safely ignorable (forward compatibility)")
    void unknownKindSafelyIgnorable() throws IOException {
        Path unknown = CORPUS_DIR.resolve("90-compatibility-unknown-kind.json");
        Assumptions.assumeTrue(Files.exists(unknown), "unknown-kind fixture not found");
        JsonNode env = MAPPER.readTree(unknown.toFile());
        assertEquals("custom.future_event", env.get("event").get("type").asText());
    }

    @Test
    @DisplayName("EventKind enum covers all non-edge-case corpus kinds")
    void eventKindCoversAllCorpusKinds() throws IOException {
        List<String> missing = new ArrayList<>();
        for (Path f : corpusFiles) {
            if (f.getFileName().toString().startsWith("9")) continue;
            JsonNode env = MAPPER.readTree(f.toFile());
            String kind = env.get("event").get("type").asText();
            try {
                EventKind.fromValue(kind);
            } catch (IllegalArgumentException e) {
                missing.add(kind);
            }
        }
        assertTrue(missing.isEmpty(),
                "EventKind enum missing values: " + String.join(", ", missing));
    }

    @Test
    @DisplayName("input.ack carries mode and durability so a buffered accept is not read as delivered")
    void inputAckReportsModeAndDurability() throws IOException {
        Path buffered = CORPUS_DIR.resolve("93-compatibility-input-ack-buffered.json");
        Assumptions.assumeTrue(Files.exists(buffered), "buffered fixture not found");
        JsonNode env = MAPPER.readTree(buffered.toFile());
        InputAckData ack = MAPPER.treeToValue(env.get("event").get("data"), InputAckData.class);

        assertEquals("accepted", ack.getStatus());
        assertEquals("buffered", ack.getInputMode());
        assertEquals("volatile", ack.getDurability());
        assertEquals("exec_parent_1", ack.getParentExecutionId());
        // The volatile acceptance is explicitly NOT a delivery.
        assertNotEquals("delivered", ack.getStatus());
    }

    @Test
    @DisplayName("input.ack from a pre-receipt server decodes with the fields absent")
    void inputAckLegacyPayloadWithoutModeAndDurability() throws IOException {
        Path legacy = CORPUS_DIR.resolve("94-compatibility-input-ack-legacy.json");
        Assumptions.assumeTrue(Files.exists(legacy), "legacy fixture not found");
        JsonNode env = MAPPER.readTree(legacy.toFile());
        InputAckData ack = MAPPER.treeToValue(env.get("event").get("data"), InputAckData.class);

        assertEquals("delivered", ack.getStatus());
        assertNull(ack.getInputMode());
        assertNull(ack.getDurability());
        assertNull(ack.getParentExecutionId());
    }
}
