package kotlinextractor

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extractAST(t *testing.T, src string, isAndroid bool) []facts.Fact {
	t.Helper()
	return extractFileAST([]byte(src), "pkg/test.kt", isAndroid, "", "")
}

func findFact(ff []facts.Fact, name string) (facts.Fact, bool) {
	for _, f := range ff {
		if f.Name == name {
			return f, true
		}
	}
	return facts.Fact{}, false
}

func findFactsByKind(ff []facts.Fact, kind string) []facts.Fact {
	var result []facts.Fact
	for _, f := range ff {
		if f.Kind == kind {
			result = append(result, f)
		}
	}
	return result
}

func hasRelation(f facts.Fact, relKind, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == relKind && r.Target == target {
			return true
		}
	}
	return false
}

// --- Parity with the legacy regex extractor (covers TestExtract_* cases) ---

func TestAST_DataClass(t *testing.T) {
	ff := extractAST(t, `
package pkg
data class User(val name: String, val email: String) {
}
`, false)
	f, ok := findFact(ff, "pkg.User")
	if !ok {
		t.Fatal("expected fact for pkg.User")
	}
	if f.Props["data_class"] != true {
		t.Errorf("data_class = %v, want true", f.Props["data_class"])
	}
	if f.Props["symbol_kind"] != facts.SymbolClass {
		t.Errorf("symbol_kind = %v, want class", f.Props["symbol_kind"])
	}
}

func TestAST_SealedClass(t *testing.T) {
	ff := extractAST(t, `
package pkg
sealed class Result {
}
`, false)
	f, ok := findFact(ff, "pkg.Result")
	if !ok {
		t.Fatal("expected fact for pkg.Result")
	}
	if f.Props["sealed"] != true {
		t.Errorf("sealed = %v, want true", f.Props["sealed"])
	}
}

func TestAST_InterfaceDeclaration(t *testing.T) {
	ff := extractAST(t, `
package pkg
interface UserRepository {
    suspend fun getUsers(): List<User>
}
`, false)
	f, ok := findFact(ff, "pkg.UserRepository")
	if !ok {
		t.Fatal("expected fact for pkg.UserRepository")
	}
	if f.Props["symbol_kind"] != facts.SymbolInterface {
		t.Errorf("symbol_kind = %v, want interface", f.Props["symbol_kind"])
	}
}

func TestAST_ObjectDeclaration(t *testing.T) {
	ff := extractAST(t, `
package pkg
object AppModule : Module {
}
`, false)
	f, ok := findFact(ff, "pkg.AppModule")
	if !ok {
		t.Fatal("expected fact for pkg.AppModule")
	}
	if f.Props["object"] != true {
		t.Errorf("object = %v, want true", f.Props["object"])
	}
	if !hasRelation(f, facts.RelImplements, "Module") {
		t.Error("expected implements relation for Module")
	}
}

func TestAST_SuspendFunction(t *testing.T) {
	ff := extractAST(t, `
package pkg
suspend fun fetchUsers() {
}
`, false)
	f, ok := findFact(ff, "pkg.fetchUsers")
	if !ok {
		t.Fatal("expected fact for pkg.fetchUsers")
	}
	if f.Props["suspend"] != true {
		t.Errorf("suspend = %v, want true", f.Props["suspend"])
	}
}

func TestAST_ComposableFunction(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Composable
fun HomeScreen() {
}
`, true)
	f, ok := findFact(ff, "pkg.HomeScreen")
	if !ok {
		t.Fatal("expected fact for pkg.HomeScreen")
	}
	if f.Props["android_component"] != "composable" {
		t.Errorf("android_component = %v, want composable", f.Props["android_component"])
	}
}

func TestAST_HiltViewModel(t *testing.T) {
	ff := extractAST(t, `
package pkg
@HiltViewModel
class HomeViewModel @Inject constructor(
    private val repo: UserRepository
) : ViewModel() {
}
`, true)
	f, ok := findFact(ff, "pkg.HomeViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.HomeViewModel")
	}
	if f.Props["android_component"] != "viewmodel" {
		t.Errorf("android_component = %v, want viewmodel", f.Props["android_component"])
	}
	if !hasRelation(f, facts.RelImplements, "ViewModel") {
		t.Error("expected implements relation for ViewModel")
	}
}

func TestAST_RoomStorage(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Entity
class UserEntity {
}
`, true)
	st := findFactsByKind(ff, facts.KindStorage)
	if len(st) != 1 {
		t.Fatalf("expected 1 storage fact, got %d", len(st))
	}
	if st[0].Props["storage_kind"] != "entity" {
		t.Errorf("storage_kind = %v, want entity", st[0].Props["storage_kind"])
	}
}

func TestAST_Imports(t *testing.T) {
	ff := extractAST(t, `
package pkg
import kotlinx.coroutines.flow.Flow
import android.os.Bundle
`, false)
	deps := findFactsByKind(ff, facts.KindDependency)
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependency facts, got %d (got: %+v)", len(deps), deps)
	}
}

// --- New edge types: RelInjects and RelInstantiates ---

func TestAST_InjectConstructor_EmitsInjects(t *testing.T) {
	ff := extractAST(t, `
package pkg
class JournalRepository @Inject constructor(
    private val api: ApiService,
    private val dao: JournalDao,
) {
}
`, false)
	f, ok := findFact(ff, "pkg.JournalRepository")
	if !ok {
		t.Fatal("expected fact for pkg.JournalRepository")
	}
	if !hasRelation(f, facts.RelInjects, "ApiService") {
		t.Error("expected RelInjects → ApiService")
	}
	if !hasRelation(f, facts.RelInjects, "JournalDao") {
		t.Error("expected RelInjects → JournalDao")
	}
}

func TestAST_HiltViewModel_InjectsParamTypes(t *testing.T) {
	ff := extractAST(t, `
package pkg
@HiltViewModel
class HomeViewModel @Inject constructor(
    private val repo: UserRepository,
    private val useCase: GetUsersUseCase,
) : ViewModel()
`, true)
	f, ok := findFact(ff, "pkg.HomeViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.HomeViewModel")
	}
	if !hasRelation(f, facts.RelInjects, "UserRepository") {
		t.Error("expected RelInjects → UserRepository")
	}
	if !hasRelation(f, facts.RelInjects, "GetUsersUseCase") {
		t.Error("expected RelInjects → GetUsersUseCase")
	}
}

// The motivating case: a ViewModel that bypasses Hilt by direct-instantiating
// a UseCase as a constructor-parameter default. The legacy regex extractor
// missed this entirely; the AST extractor must surface the call site as a
// RelInstantiates edge so reverse-dependency queries on JournalCodecUseCase
// resolve back to JournalHistoryViewModel.
func TestAST_DirectInstantiation_InCtorParamDefault(t *testing.T) {
	ff := extractAST(t, `
package pkg
class JournalHistoryViewModel(
    private val journalCodecUseCase: JournalCodecUseCase = JournalCodecUseCase(),
) {
}
`, false)
	f, ok := findFact(ff, "pkg.JournalHistoryViewModel")
	if !ok {
		t.Fatal("expected fact for pkg.JournalHistoryViewModel")
	}
	if !hasRelation(f, facts.RelInstantiates, "JournalCodecUseCase") {
		t.Errorf("expected RelInstantiates → JournalCodecUseCase, got relations: %+v", f.Relations)
	}
}

func TestAST_DirectInstantiation_InPropertyInitializer(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Foo {
    private val codec = JournalCodecUseCase()
}
`, false)
	f, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	if !hasRelation(f, facts.RelInstantiates, "JournalCodecUseCase") {
		t.Errorf("expected RelInstantiates → JournalCodecUseCase, got %+v", f.Relations)
	}
}

func TestAST_HiltProvidesEmitsInstantiates(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Module
@InstallIn(SingletonComponent::class)
object UseCaseModule {
    @Provides
    @Singleton
    fun provideJournalCodecUseCase(): JournalCodecUseCase = JournalCodecUseCase()
}
`, true)
	f, ok := findFact(ff, "pkg.provideJournalCodecUseCase")
	if !ok {
		t.Fatal("expected fact for pkg.provideJournalCodecUseCase (function)")
	}
	if !hasRelation(f, facts.RelInstantiates, "JournalCodecUseCase") {
		t.Errorf("expected RelInstantiates → JournalCodecUseCase on Hilt provider, got %+v", f.Relations)
	}
}

// Lowercase-prefix calls (regular function calls) are NOT treated as constructors,
// so they do not produce stray RelInstantiates edges.
func TestAST_LowercaseFunctionCall_NoInstantiate(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Foo {
    private val v = computeSomething()
}
`, false)
	f, ok := findFact(ff, "pkg.Foo")
	if !ok {
		t.Fatal("expected fact for pkg.Foo")
	}
	for _, r := range f.Relations {
		if r.Kind == facts.RelInstantiates {
			t.Errorf("unexpected RelInstantiates edge: %+v", r)
		}
	}
}

// --- Additional parity coverage previously held by the deleted legacy tests ---

func TestAST_MultiLineConstructor(t *testing.T) {
	ff := extractAST(t, `
package pkg
class UserRepository(
    private val api: ApiService,
    private val db: UserDao
) : Repository {
}
`, true)
	f, ok := findFact(ff, "pkg.UserRepository")
	if !ok {
		t.Fatal("expected fact for pkg.UserRepository")
	}
	if !hasRelation(f, facts.RelImplements, "Repository") {
		t.Error("expected implements relation for Repository")
	}
	if f.Props["android_component"] != "repository" {
		t.Errorf("android_component = %v, want repository", f.Props["android_component"])
	}
}

func TestAST_NoRoomStorage_WithoutAndroid(t *testing.T) {
	ff := extractAST(t, `
package pkg
@Entity
class UserEntity {
}
`, false)
	if st := findFactsByKind(ff, facts.KindStorage); len(st) != 0 {
		t.Errorf("expected 0 storage facts when isAndroid=false, got %d", len(st))
	}
}

func TestAST_EnumClass(t *testing.T) {
	ff := extractAST(t, `
package pkg
enum class Direction {
    NORTH, SOUTH, EAST, WEST
}
`, false)
	f, ok := findFact(ff, "pkg.Direction")
	if !ok {
		t.Fatal("expected fact for pkg.Direction")
	}
	if f.Props["enum"] != true {
		t.Errorf("enum = %v, want true", f.Props["enum"])
	}
}

// Constructor calls reached via navigation expressions like `pkg.Foo()` should
// still surface the simple type name.
func TestAST_QualifiedConstructorCall(t *testing.T) {
	ff := extractAST(t, `
package pkg
class Wrapper {
    private val x = some.namespace.Bar()
}
`, false)
	f, ok := findFact(ff, "pkg.Wrapper")
	if !ok {
		t.Fatal("expected fact for pkg.Wrapper")
	}
	if !hasRelation(f, facts.RelInstantiates, "Bar") {
		t.Errorf("expected RelInstantiates → Bar (last segment of qualified callee), got %+v", f.Relations)
	}
}
